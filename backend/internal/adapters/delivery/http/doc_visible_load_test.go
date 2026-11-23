//go:build postgres

package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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
type rangeLoadGenerationRead struct {
	outbound.DocRangeGenerationRead
}

func (storage *rangeLoadVisibleStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	return &rangeLoadGenerationRead{read}, err
}

func (read *rangeLoadGenerationRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	if cost := rangeLoadCost(ctx); cost != nil {
		cost.Pages++
		if cost.Pages > 20 {
			return nil, domain.ErrDocumentResourceLimit
		}
	}
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

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

func (storage *rangeLoadVisibleStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	return storage.PostgresStore.ReadVerifiedDoc(ctx, repo, hash)
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

func (storage *rangeLoadVisibleStore) DocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return domain.DocRangeIndex{}, err
	}
	return storage.PostgresStore.DocRangeIndex(ctx, repo, hash)
}

func (storage *rangeLoadVisibleStore) DocRangePage(ctx context.Context, repo domain.ContentHash, header domain.DocRangeIndex, position int) ([]domain.DocRangeEntry, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return nil, err
	}
	return storage.PostgresStore.DocRangePage(ctx, repo, header, position)
}

func (storage *rangeLoadVisibleStore) OpenDocRange(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeRead, error) {
	if err := rangeLoadDenyGather(ctx); err != nil {
		return nil, err
	}
	return storage.PostgresStore.OpenDocRange(ctx, repo, hash)
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

func TestDocVisibleLoadGatherGuards(t *testing.T) {
	storage := &rangeLoadVisibleStore{}
	cost := &rangeLoadVisibleCost{}
	ctx := context.WithValue(context.Background(), rangeLoadVisibleCostKey{}, cost)
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	checks := []func() error{
		func() error { _, err := storage.GetDoc(ctx, repo, hash); return err },
		func() error { _, err := storage.ReadVerifiedDoc(ctx, repo, hash); return err },
		func() error { _, err := storage.VerifyStoredDoc(ctx, repo, hash); return err },
		func() error { _, err := storage.GetChunk(ctx, repo, hash); return err },
		func() error { _, err := storage.GetDocManifest(ctx, repo, hash); return err },
		func() error { _, err := storage.DocReadIndex(ctx, repo, hash); return err },
		func() error { _, err := storage.DocRangeIndex(ctx, repo, hash); return err },
		func() error { _, err := storage.DocRangePage(ctx, repo, domain.DocRangeIndex{}, 0); return err },
		func() error { _, err := storage.OpenDocRange(ctx, repo, hash); return err },
	}
	for _, check := range checks {
		if check() == nil {
			t.Fatal("whole-document gather was allowed")
		}
	}
	if cost.Forbidden != len(checks) {
		t.Fatal("gather counters missing", cost)
	}
}
