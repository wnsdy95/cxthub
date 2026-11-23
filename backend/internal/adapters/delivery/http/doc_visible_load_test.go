//go:build postgres

package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rangeLoadVisibleCost struct {
	View, Kind                                  string
	Start                                       time.Time
	Units, Pages, Chunks, ChunkBytes, Forbidden int
	Error                                       string `json:",omitempty"`
}

type rangeLoadVisibleCostKey struct{}

func rangeLoadCost(ctx context.Context) *rangeLoadVisibleCost {
	cost, _ := ctx.Value(rangeLoadVisibleCostKey{}).(*rangeLoadVisibleCost)
	return cost
}

func rangeLoadDenyGather(ctx context.Context) error {
	if cost := rangeLoadCost(ctx); cost != nil {
		cost.Forbidden++
		return errors.New("visible-v1 attempted whole-document or legacy fallback read")
	}
	return nil
}

type rangeLoadVisibleStore struct{ *store.PostgresStore }

func (storage *rangeLoadVisibleStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	if cost := rangeLoadCost(ctx); cost != nil {
		cost.Chunks++
		cost.ChunkBytes += int(length)
		if length <= 0 || length > domain.ConversationManifestChunkBytes || cost.Chunks > 20 || cost.ChunkBytes > 20*domain.ConversationManifestChunkBytes {
			return nil, domain.ErrDocumentResourceLimit
		}
	}
	return storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (storage *rangeLoadVisibleStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.SessionDoc{}, err
	}
	return storage.PostgresStore.GetDoc(ctx, repo, hash)
}

func (storage *rangeLoadVisibleStore) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
}

func (storage *rangeLoadVisibleStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return nil, err
	}
	return storage.PostgresStore.GetChunk(ctx, repo, hash)
}

func (storage *rangeLoadVisibleStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return storage.PostgresStore.GetDocManifest(ctx, repo, hash)
}

func (storage *rangeLoadVisibleStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.DocReadIndex{}, err
	}
	return storage.PostgresStore.DocReadIndex(ctx, repo, hash)
}

func rangeLoadVisibleExpected(doc rangeLoadDocument, view string, start, limit int) ([]domain.DocVisibleUnit, int) {
	units := []domain.DocVisibleUnit{}
	first, ordinal, turn := 0, 0, -1
	for position := 0; position < len(doc.Events) && len(units) < limit; {
		event := doc.Events[position]
		prompt := event.Kind == domain.EventMessage && event.Role == domain.RoleUser
		if prompt {
			turn = position
		}
		unit := domain.DocVisibleUnit{From: position, To: position + 1, TurnIndex: turn, EventIndex: -1}
		switch view {
		case "all":
			unit.EventIndex = position
		case "chat":
			for {
				if doc.Events[unit.To-1].Kind == domain.EventMessage {
					unit.EventIndex = unit.To - 1
					break
				}
				if unit.To == len(doc.Events) || doc.Events[unit.To].Role == domain.RoleUser {
					break
				}
				unit.To++
			}
		case "prompts":
			if prompt {
				unit.EventIndex = position
			}
			for unit.To < len(doc.Events) && doc.Events[unit.To].Role != domain.RoleUser {
				unit.To++
			}
		default:
			panic("invalid fixture view")
		}
		position = unit.To
		if unit.To > start {
			if len(units) == 0 {
				first = ordinal
			}
			unit.From = max(unit.From, start)
			if unit.EventIndex < unit.From {
				unit.EventIndex = -1
			}
			unit.FoldRows = unit.To - unit.From
			if unit.EventIndex >= 0 {
				unit.FoldRows--
			}
			units = append(units, unit)
		}
		ordinal++
	}
	return units, first
}

func TestDocVisibleLoadFixtureOracle(t *testing.T) {
	doc := rangeLoadBuild(t, "visible-oracle", 2<<20, func(domain.ContentHash, []byte) error { return nil })
	for _, test := range []struct {
		view                                        string
		start, from, to, event, turn, rows, ordinal int
	}{
		{"all", 1, 1, 2, 1, 0, 0, 1},
		{"chat", 1, 1, 140, -1, 0, 139, 1},
		{"chat", 142, 142, 146, 145, 140, 3, 3},
		{"prompts", 0, 0, 140, 0, 0, 139, 0},
		{"prompts", 1, 1, 140, -1, 0, 139, 0},
		{"prompts", 142, 142, 150, -1, 140, 8, 1},
	} {
		units, ordinal := rangeLoadVisibleExpected(doc, test.view, test.start, 20)
		if len(units) != 20 || ordinal != test.ordinal {
			t.Fatalf("%+v count/ordinal=%d/%d", test, len(units), ordinal)
		}
		unit := units[0]
		if unit.From != test.from || unit.To != test.to || unit.EventIndex != test.event || unit.TurnIndex != test.turn || unit.FoldRows != test.rows {
			t.Fatalf("%+v unit=%+v", test, unit)
		}
	}
}
