//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rangeLoadOperation struct {
	Phase, Kind  string
	Start, End   time.Time
	Microseconds int64
	Attempts     int
	Completed    bool
	Error        string `json:",omitempty"`
}

func rangeLoadVisibleKinds() []string {
	kinds := []string{}
	for _, view := range []string{"all", "chat", "prompts"} {
		kinds = append(kinds, "visible_"+view+"_initial", "visible_"+view+"_continuation")
		if view != "all" {
			kinds = append(kinds, "visible_"+view+"_fold_initial", "visible_"+view+"_fold_continuation")
		}
	}
	return kinds
}

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
type rangeLoadVisibleRead struct {
	outbound.DocVisibleGenerationRead
}
type rangeLoadGenerationRead struct {
	outbound.DocRangeGenerationRead
}

func (storage *rangeLoadVisibleStore) OpenDocVisibleGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	read, err := storage.PostgresStore.OpenDocVisibleGeneration(ctx, repo, hash)
	return &rangeLoadVisibleRead{read}, err
}

func (read *rangeLoadVisibleRead) Unit(ctx context.Context, view string, position int, byEvent bool) (domain.DocVisibleUnit, int, error) {
	if cost := rangeLoadCost(ctx); cost != nil {
		cost.Units++
		if cost.Units > 21 {
			return domain.DocVisibleUnit{}, 0, domain.ErrDocumentResourceLimit
		}
	}
	return read.DocVisibleGenerationRead.Unit(ctx, view, position, byEvent)
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

type rangeLoadVisibleBackend struct {
	*app.Service
	mu    sync.Mutex
	costs []rangeLoadVisibleCost
}

func (backend *rangeLoadVisibleBackend) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	cost := rangeLoadVisibleCost{View: request.View, Kind: "initial", Start: time.Now()}
	maxUnits, maxPages := 21, 20
	if request.Cursor != "" || request.Expand != "" {
		token := request.Cursor
		cost.Kind, maxUnits = "continuation", 20
		if request.Expand != "" {
			token, cost.Kind, maxUnits, maxPages = request.Expand, "fold", 1, 2
		}
		selector, err := domain.DecodeDocVisibleSelector(token)
		if err != nil {
			return domain.DocVisiblePage{}, err
		}
		cost.View = selector.View
	}
	page, err := backend.Service.ReadDocVisiblePage(context.WithValue(ctx, rangeLoadVisibleCostKey{}, &cost), repo, hash, request)
	if cost.Forbidden != 0 || cost.Units > maxUnits || cost.Pages > maxPages || cost.Chunks > 20 || cost.ChunkBytes > 20*domain.ConversationManifestChunkBytes {
		err = errors.Join(err, fmt.Errorf("visible read cost exceeded: %+v", cost))
	}
	if err == nil && (cost.Units == 0 || cost.Pages == 0) {
		err = errors.New("visible bounded read instrumentation was bypassed")
	}
	if err != nil {
		cost.Error = err.Error()
	}
	backend.mu.Lock()
	backend.costs = append(backend.costs, cost)
	backend.mu.Unlock()
	return page, err
}

type rangeLoadVisibleUnit struct {
	From, To  int
	TurnIndex int `json:"turn_index"`
	Event     json.RawMessage
	Fold      *domain.DocVisibleFold
}

type rangeLoadVisiblePage struct {
	Projection, Kind, View                   string
	Hash, Base, Generation                   domain.ContentHash
	Rules, Total, Inherited, Start, From, To int
	Envelope                                 domain.CIREnvelope
	Units                                    []rangeLoadVisibleUnit
	Fold                                     *domain.DocVisibleFoldRange
	Events                                   []json.RawMessage
	NextCursor                               *string `json:"next_cursor"`
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

func rangeLoadCheckVisibleEvent(raw json.RawMessage, doc rangeLoadDocument, position int) error {
	var event domain.CIREvent
	var metadata struct {
		Index    *int  `json:"event_index"`
		Bytes    int   `json:"event_bytes"`
		Deferred *bool `json:"deferred"`
		Seed     *bool `json:"seed_summary"`
		Empty    *bool `json:"empty_reasoning"`
	}
	if json.Unmarshal(raw, &event) != nil || json.Unmarshal(raw, &metadata) != nil || position < 0 || position >= len(doc.Events) {
		return errors.New("invalid visible event")
	}
	want := doc.Events[position]
	if metadata.Index == nil || *metadata.Index != position || metadata.Bytes != want.Bytes || metadata.Seed == nil || *metadata.Seed || metadata.Empty == nil || *metadata.Empty || event.Seq != position || event.Kind != want.Kind || event.Role != want.Role {
		return fmt.Errorf("visible event ordinal/metadata mismatch at %d", position)
	}
	if metadata.Deferred != nil && *metadata.Deferred {
		if want.Kind != domain.EventToolResult || len(event.Blocks) != 0 || event.Output != nil {
			return fmt.Errorf("invalid deferred visible body at %d", position)
		}
	} else {
		if want.Kind == domain.EventToolResult {
			return errors.New("visible tool body was eagerly gathered")
		}
		canonical, err := rangeLoadCanonicalEvent(event)
		if err != nil || domain.HashContent(canonical) != want.Hash {
			return fmt.Errorf("visible inline checksum mismatch at %d", position)
		}
	}
	return nil
}

func rangeLoadVisibleSelector(page rangeLoadVisiblePage, repo domain.ContentHash, purpose string, unit, next int) domain.DocVisibleSelector {
	return domain.DocVisibleSelector{Version: 1, Purpose: purpose, Repo: repo, Hash: page.Hash, Generation: page.Generation, View: page.View, Start: page.Start, Total: page.Total, Unit: unit, Next: next, Limit: 20}
}

func rangeLoadCheckSelector(token string, want domain.DocVisibleSelector) error {
	actual, err := domain.DecodeDocVisibleSelector(token)
	if err != nil || actual != want {
		return fmt.Errorf("visible selector binding/ordinal mismatch: actual=%+v want=%+v err=%v", actual, want, err)
	}
	return nil
}

func rangeLoadDecodeVisible(raw []byte, doc rangeLoadDocument, view, kind string, start, from int, generation domain.ContentHash) (rangeLoadVisiblePage, error) {
	var page rangeLoadVisiblePage
	var envelope domain.CIREnvelope
	if len(raw) > 128<<10 || json.Unmarshal(raw, &page) != nil || json.Unmarshal(doc.Manifest.Envelope, &envelope) != nil {
		return page, errors.New("visible JSON/budget failure")
	}
	if page.Projection != domain.DocVisibleProjection || page.Kind != kind || page.Hash != doc.Wire.Hash || page.Base != "" || page.View != view || page.Rules != domain.DocVisibleAlgorithm || page.Total != len(doc.Events) || page.Inherited != 0 || page.Start != start || page.From != from || page.To <= from || page.To > page.Total || domain.ValidateContentHash(page.Generation) != nil || generation != "" && page.Generation != generation || !reflect.DeepEqual(page.Envelope, envelope) {
		return page, errors.New("visible identity/range/generation mismatch")
	}
	return page, nil
}

func rangeLoadCheckVisibleUnits(raw []byte, fixture rangeLoadFixture, view string, start, from int, generation domain.ContentHash) (rangeLoadVisiblePage, error) {
	page, err := rangeLoadDecodeVisible(raw, fixture.Root, view, "units", start, from, generation)
	if err != nil {
		return page, err
	}
	expected, ordinal := rangeLoadVisibleExpected(fixture.Root, view, from, 20)
	if page.Fold != nil || len(page.Events) != 0 || len(page.Units) != len(expected) || len(expected) == 0 {
		return page, errors.New("visible unit count/shape mismatch")
	}
	for index, want := range expected {
		unit := page.Units[index]
		if unit.From != want.From || unit.To != want.To || unit.TurnIndex != want.TurnIndex {
			return page, fmt.Errorf("visible unit boundary/turn mismatch at %d", index)
		}
		if want.EventIndex >= 0 {
			if err := rangeLoadCheckVisibleEvent(unit.Event, fixture.Root, want.EventIndex); err != nil {
				return page, err
			}
		} else if string(unit.Event) != "null" {
			return page, errors.New("fold-only unit exposed an event")
		}
		if want.FoldRows == 0 {
			if unit.Fold != nil {
				return page, errors.New("unexpected visible fold")
			}
			continue
		}
		from, to := want.From, want.To
		if want.EventIndex == from {
			from++
		} else if want.EventIndex == to-1 {
			to--
		}
		if unit.Fold == nil || unit.Fold.From != from || unit.Fold.To != to || unit.Fold.Rows != want.FoldRows {
			return page, errors.New("visible fold boundary/count mismatch")
		}
		if err := rangeLoadCheckSelector(unit.Fold.Cursor, rangeLoadVisibleSelector(page, fixture.Repo, "fold", ordinal+index, from)); err != nil {
			return page, err
		}
	}
	if page.To != expected[len(expected)-1].To || (page.NextCursor == nil) != (page.To == page.Total) {
		return page, errors.New("visible next cursor progress/completion mismatch")
	}
	if page.NextCursor != nil {
		err = rangeLoadCheckSelector(*page.NextCursor, rangeLoadVisibleSelector(page, fixture.Repo, "units", ordinal+len(expected), page.To))
	}
	return page, err
}

func rangeLoadCheckVisibleFold(raw []byte, fixture rangeLoadFixture, main rangeLoadVisiblePage, fold domain.DocVisibleFold, from int) (rangeLoadVisiblePage, error) {
	page, err := rangeLoadDecodeVisible(raw, fixture.Root, main.View, "fold", main.Start, from, main.Generation)
	if err != nil {
		return page, err
	}
	if page.Fold == nil || page.Fold.From != fold.From || page.Fold.To != fold.To || page.To != min(from+domain.DocVisiblePageUnits, fold.To) || len(page.Units) != 0 || len(page.Events) != page.To-from || (page.NextCursor == nil) != (page.To == fold.To) {
		return page, errors.New("fold boundary/count/continuation mismatch")
	}
	for ordinal, event := range page.Events {
		if err := rangeLoadCheckVisibleEvent(event, fixture.Root, from+ordinal); err != nil {
			return page, err
		}
	}
	if page.NextCursor != nil {
		selector, err := domain.DecodeDocVisibleSelector(fold.Cursor)
		if err != nil {
			return page, err
		}
		selector.Next = page.To
		if err := rangeLoadCheckSelector(*page.NextCursor, selector); err != nil {
			return page, err
		}
	}
	return page, nil
}

func (client *rangeLoadClient) readVisible(ctx context.Context, phase, view string, fixture rangeLoadFixture, iteration int) error {
	start := 0
	if iteration%3 == 1 {
		start = 1
	} else if iteration%3 == 2 {
		start = len(fixture.Root.Events) - 600
	}
	path := "/docs/" + string(fixture.Root.Wire.Hash) + "/events?projection=visible-v1&"
	query := fmt.Sprintf("view=%s&offset=%d&limit=20", view, start)
	kind, from, generation := "initial", start, domain.ContentHash("")
	for pageNumber := 0; pageNumber < 2; pageNumber++ {
		raw, err := client.request(ctx, phase, "visible_"+view+"_"+kind, "GET", path+query, nil, 200)
		if err != nil {
			return err
		}
		page, err := rangeLoadCheckVisibleUnits(raw, fixture, view, start, from, generation)
		if err != nil {
			return fmt.Errorf("visible %s %s: %w", view, kind, err)
		}
		if page.NextCursor == nil {
			return errors.New("fixture exhausted before representative continuation")
		}
		if pageNumber == 0 {
			for _, unit := range page.Units {
				if unit.Fold == nil {
					continue
				}
				fold := *unit.Fold
				token, position, foldKind := fold.Cursor, fold.From, "fold_initial"
				for attempt := 0; attempt < 3 && position < fold.To; attempt++ {
					body, err := client.request(ctx, phase, "visible_"+view+"_"+foldKind, "GET", path+"expand="+url.QueryEscape(token), nil, 200)
					if err != nil {
						return err
					}
					part, err := rangeLoadCheckVisibleFold(body, fixture, page, fold, position)
					if err != nil {
						return err
					}
					position = part.To
					if part.NextCursor != nil {
						token = *part.NextCursor
					}
					foldKind = "fold_continuation"
				}
				if position != fold.To {
					return errors.New("fold request bound exhausted")
				}
				break
			}
		}
		query, kind, from, generation = "cursor="+url.QueryEscape(*page.NextCursor), "continuation", page.To, page.Generation
	}
	return nil
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

func TestDocVisibleLoadWireValidation(t *testing.T) {
	stream := []byte{}
	doc := rangeLoadBuild(t, "visible-wire", 2<<20, func(_ domain.ContentHash, raw []byte) error {
		stream = append(stream, raw...)
		return nil
	})
	var events []domain.CIREvent
	if err := json.Unmarshal(append(append([]byte("["), stream...), ']'), &events); err != nil {
		t.Fatal(err)
	}
	fixture := rangeLoadFixture{Repo: domain.HashContent([]byte("visible-wire-repo")), Root: doc}
	generation := domain.HashContent([]byte("visible-wire-generation"))
	eventWire := func(position int) json.RawMessage {
		event := events[position]
		deferred := event.Kind == domain.EventToolResult
		if deferred {
			event = domain.CIREvent{Seq: position, Kind: event.Kind, Role: event.Role}
		}
		raw, err := json.Marshal(domain.DocVisibleEvent{DocViewEvent: domain.DocViewEvent{Event: event, Index: position, Bytes: doc.Events[position].Bytes, Deferred: deferred}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var envelope domain.CIREnvelope
	if err := json.Unmarshal(doc.Manifest.Envelope, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"all", "chat", "prompts"} {
		for _, start := range []int{0, 1, 142} {
			page := rangeLoadVisiblePage{Projection: domain.DocVisibleProjection, Kind: "units", View: view, Hash: doc.Wire.Hash, Generation: generation, Rules: domain.DocVisibleAlgorithm, Total: len(doc.Events), Start: start, From: start, Envelope: envelope}
			units, ordinal := rangeLoadVisibleExpected(doc, view, start, 20)
			for index, want := range units {
				unit := rangeLoadVisibleUnit{From: want.From, To: want.To, TurnIndex: want.TurnIndex, Event: json.RawMessage("null")}
				if want.EventIndex >= 0 {
					unit.Event = eventWire(want.EventIndex)
				}
				if want.FoldRows > 0 {
					from, to := want.From, want.To
					if want.EventIndex == from {
						from++
					} else if want.EventIndex == to-1 {
						to--
					}
					cursor, err := rangeLoadVisibleSelector(page, fixture.Repo, "fold", ordinal+index, from).Encode()
					if err != nil {
						t.Fatal(err)
					}
					unit.Fold = &domain.DocVisibleFold{From: from, To: to, Rows: want.FoldRows, Cursor: cursor}
				}
				page.Units = append(page.Units, unit)
				page.To = unit.To
			}
			cursor, err := rangeLoadVisibleSelector(page, fixture.Repo, "units", ordinal+len(units), page.To).Encode()
			if err != nil {
				t.Fatal(err)
			}
			page.NextCursor = &cursor
			if _, err := rangeLoadCheckVisibleUnits(encode(page), fixture, view, start, start, generation); err != nil {
				t.Fatal(view, start, err)
			}
			for _, mutate := range []func(*rangeLoadVisiblePage){
				func(page *rangeLoadVisiblePage) { page.Units[0].From++ },
				func(page *rangeLoadVisiblePage) { page.Units[0].TurnIndex++ },
				func(page *rangeLoadVisiblePage) { page.To++ },
				func(page *rangeLoadVisiblePage) { page.NextCursor = nil },
				func(page *rangeLoadVisiblePage) { page.Generation = domain.HashContent([]byte("wrong")) },
				func(page *rangeLoadVisiblePage) {
					token, _ := rangeLoadVisibleSelector(*page, fixture.Repo, "units", ordinal, page.To).Encode()
					page.NextCursor = &token
				},
			} {
				var changed rangeLoadVisiblePage
				if err := json.Unmarshal(encode(page), &changed); err != nil {
					t.Fatal(err)
				}
				mutate(&changed)
				if _, err := rangeLoadCheckVisibleUnits(encode(changed), fixture, view, start, start, generation); err == nil {
					t.Fatal("corrupted visible unit accepted", view, start)
				}
			}
			for _, unit := range page.Units {
				if unit.Fold == nil {
					continue
				}
				fold := *unit.Fold
				for from := fold.From; from < fold.To; {
					part := page
					part.Kind, part.From, part.To, part.Units, part.NextCursor = "fold", from, min(from+domain.DocVisiblePageUnits, fold.To), nil, nil
					part.Fold = &domain.DocVisibleFoldRange{From: fold.From, To: fold.To}
					for position := from; position < part.To; position++ {
						part.Events = append(part.Events, eventWire(position))
					}
					if part.To < fold.To {
						selector, _ := domain.DecodeDocVisibleSelector(fold.Cursor)
						selector.Next = part.To
						token, _ := selector.Encode()
						part.NextCursor = &token
					}
					if _, err := rangeLoadCheckVisibleFold(encode(part), fixture, page, fold, from); err != nil {
						t.Fatal(err)
					}
					part.Events[0] = eventWire(from + 1)
					if _, err := rangeLoadCheckVisibleFold(encode(part), fixture, page, fold, from); err == nil {
						t.Fatal("duplicated fold event ordinal accepted")
					}
					from = part.To
				}
				break
			}
		}
	}
	for _, field := range []string{"event_index", "seed_summary", "empty_reasoning", "deferred"} {
		position := 0
		if field == "deferred" {
			position = 1
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(eventWire(position), &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, field)
		if err := rangeLoadCheckVisibleEvent(encode(fields), doc, position); err == nil {
			t.Fatal("missing flattened field accepted", field)
		}
	}
}
