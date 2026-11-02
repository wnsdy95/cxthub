package documentquery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const eventPrefixEnvelopeJSON = `{"captured_at":"2026-10-26T00:00:00Z","cir_version":"1","cwd":"/event-prefix","fidelity":"full","git_branch":"main","session_origin_id":"event-prefix","source_model":"fixture","source_provider":"codex"}`

var eventPrefixGoldenJSON = []string{
	"{\"blocks\":[{\"text\":\"\uc55e\ub9d0 NEEDLE\",\"type\":\"text\"}],\"kind\":\"message\",\"role\":\"user\",\"seq\":10}",
	"{\"blocks\":[{\"text\":\"\ub450\ubc88\uc9f8 🌿 needle\",\"type\":\"text\"}],\"kind\":\"message\",\"role\":\"assistant\",\"seq\":20}",
	`{"call_id":"call-1","input":{"text":"needle tool input"},"kind":"tool_call","seq":30,"tool_name":"lookup"}`,
	"{\"cross_replayable\":false,\"kind\":\"reasoning\",\"redacted_summary\":\"Needle \uc0dd\uac01\",\"seq\":40}",
	`{"call_id":"call-1","kind":"tool_result","output":"needle tool output","seq":50}`,
	"{\"blocks\":[{\"text\":\"\ub9c8\uc9c0\ub9c9 \uc904\\nNEEDLE🙂\",\"type\":\"text\"}],\"kind\":\"message\",\"role\":\"assistant\",\"seq\":60}",
	`{"kind":"turn","role":"user","seq":70}`,
}

type eventPrefixScopeKey struct{}

type eventPrefixCall struct {
	stage string
	repo  domain.ContentHash
	hash  domain.ContentHash
}

type eventPrefixLocalScope struct {
	test          *testing.T
	repo          domain.ContentHash
	active        bool
	snapshotDepth int
	calls         []eventPrefixCall
	before        func(context.Context, eventPrefixCall) error
	afterError    error
}

func (scope *eventPrefixLocalScope) record(ctx context.Context, stage string, repo, hash domain.ContentHash) error {
	call := eventPrefixCall{stage: stage, repo: repo, hash: hash}
	scope.calls = append(scope.calls, call)
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope.before != nil {
		return scope.before(ctx, call)
	}
	return nil
}

func (scope *eventPrefixLocalScope) requireBound(ctx context.Context, repo domain.ContentHash) {
	scope.test.Helper()
	if !scope.active || scope.repo != repo || ctx.Value(eventPrefixScopeKey{}) != scope {
		scope.test.Fatal("document port escaped its original repository read scope")
	}
}

func (scope *eventPrefixLocalScope) port(ctx context.Context, stage string, repo, hash domain.ContentHash) error {
	scope.requireBound(ctx, repo)
	if stage == "metadata" && scope.snapshotDepth == 0 {
		scope.test.Fatal("snapshot metadata escaped the document snapshot scope")
	}
	return scope.record(ctx, stage, repo, hash)
}

func (scope *eventPrefixLocalScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (scope *eventPrefixLocalScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (scope *eventPrefixLocalScope) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope.active {
		scope.test.Fatal("nested document admission replaced the original read scope")
	}
	if err := scope.record(ctx, "read", repo, ""); err != nil {
		return err
	}
	scope.repo, scope.active = repo, true
	defer func() { scope.active = false }()
	if err := read(context.WithValue(ctx, eventPrefixScopeKey{}, scope)); err != nil {
		return err
	}
	return scope.afterError
}

func (scope *eventPrefixLocalScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.requireBound(ctx, repo)
	if err := scope.record(ctx, "snapshot", repo, ""); err != nil {
		return err
	}
	scope.snapshotDepth++
	defer func() { scope.snapshotDepth-- }()
	return read(ctx)
}

func (scope *eventPrefixLocalScope) stages() []string {
	stages := make([]string, 0, len(scope.calls))
	for _, call := range scope.calls {
		stages = append(stages, call.stage)
	}
	return stages
}

type eventPrefixLegacy struct {
	scope     *eventPrefixLocalScope
	repo      domain.ContentHash
	chunked   bool
	documents map[domain.ContentHash]domain.SessionDoc
	manifests map[domain.ContentHash]domain.DocChunkManifest
	chunks    map[domain.ContentHash][]byte
}

func (legacy *eventPrefixLegacy) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := legacy.scope.port(ctx, "doc", repo, hash); err != nil {
		return domain.SessionDoc{}, err
	}
	document, exists := legacy.documents[hash]
	if repo != legacy.repo || !exists {
		return domain.SessionDoc{}, domain.ErrNotFound
	}
	return document, nil
}

func (legacy *eventPrefixLegacy) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := legacy.scope.port(ctx, "manifest", repo, hash); err != nil {
		return domain.DocChunkManifest{}, err
	}
	if _, exists := legacy.documents[hash]; repo != legacy.repo || !exists {
		return domain.DocChunkManifest{}, domain.ErrNotFound
	}
	manifest, exists := legacy.manifests[hash]
	if !legacy.chunked || !exists {
		return domain.DocChunkManifest{}, domain.ErrAgentHistoryUnavailable
	}
	return manifest, nil
}

func (legacy *eventPrefixLegacy) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := legacy.scope.port(ctx, "chunk", repo, hash); err != nil {
		return nil, err
	}
	body, exists := legacy.chunks[hash]
	if repo != legacy.repo || !exists {
		return nil, domain.ErrNotFound
	}
	return bytes.Clone(body), nil
}

type eventPrefixSnapshots struct {
	scope     *eventPrefixLocalScope
	repo      domain.ContentHash
	snapshots map[domain.ContentHash]domain.Snapshot
}

func (snapshots *eventPrefixSnapshots) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := snapshots.scope.port(ctx, "metadata", repo, hash); err != nil {
		return domain.Snapshot{}, err
	}
	snapshot, exists := snapshots.snapshots[hash]
	if repo != snapshots.repo || !exists {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return snapshot, nil
}

type eventPrefixDocument struct {
	document  domain.SessionDoc
	canonical []byte
	json      []string
	events    []domain.CIREvent
	plan      domain.DocChunkPlan
}

type eventPrefixFixture struct {
	test      *testing.T
	repo      domain.ContentHash
	scope     *eventPrefixLocalScope
	legacy    *eventPrefixLegacy
	snapshots *eventPrefixSnapshots
	document  eventPrefixDocument
}

func eventPrefix(test *testing.T) *eventPrefixFixture {
	test.Helper()
	return eventPrefixWithJSON(test, eventPrefixGoldenJSON)
}

func eventPrefixWithJSON(test *testing.T, eventJSON []string) *eventPrefixFixture {
	test.Helper()
	repo := domain.HashContent([]byte("event-prefix-repository"))
	scope := &eventPrefixLocalScope{test: test}
	fixture := &eventPrefixFixture{test: test, repo: repo, scope: scope,
		legacy: &eventPrefixLegacy{scope: scope, repo: repo, documents: make(map[domain.ContentHash]domain.SessionDoc),
			manifests: make(map[domain.ContentHash]domain.DocChunkManifest), chunks: make(map[domain.ContentHash][]byte)},
		snapshots: &eventPrefixSnapshots{scope: scope, repo: repo, snapshots: make(map[domain.ContentHash]domain.Snapshot)}}
	fixture.document = fixture.addDocument(eventJSON)
	return fixture
}

func (fixture *eventPrefixFixture) addDocument(eventJSON []string) eventPrefixDocument {
	fixture.test.Helper()
	var envelope domain.CIREnvelope
	if err := json.Unmarshal([]byte(eventPrefixEnvelopeJSON), &envelope); err != nil {
		fixture.test.Fatal(err)
	}
	events := make([]domain.CIREvent, len(eventJSON))
	for position, encoded := range eventJSON {
		if err := json.Unmarshal([]byte(encoded), &events[position]); err != nil {
			fixture.test.Fatal(err)
		}
	}
	input := make([]domain.CIREvent, len(events))
	for position := range events {
		input[len(events)-position-1] = events[position]
	}
	canonical := []byte(`{"envelope":` + eventPrefixEnvelopeJSON + `,"events":[` + strings.Join(eventJSON, ",") + `]}`)
	document := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: domain.CIRDocument{Envelope: envelope, Events: input}}
	actual, err := domain.CanonicalBytes(document.CIR)
	if err != nil || !bytes.Equal(actual, canonical) {
		fixture.test.Fatalf("fixture changed the original CIR canonical bytes: %v", err)
	}
	plan, chunked := domain.PlanDocChunks(canonical)
	fixture.legacy.documents[document.Hash] = document
	fixture.snapshots.snapshots[document.Hash] = domain.Snapshot{ID: document.Hash, RepoID: fixture.repo, DocHash: document.Hash}
	if chunked {
		fixture.legacy.manifests[document.Hash] = plan.Manifest
		for hash, body := range plan.Bodies {
			fixture.legacy.chunks[hash] = bytes.Clone(body)
		}
	}
	return eventPrefixDocument{document: document, canonical: canonical, json: append([]string{}, eventJSON...), events: events, plan: plan}
}

func (fixture *eventPrefixFixture) queries(atomicRead bool) *documentquery.Queries {
	fixture.test.Helper()
	queries, err := documentquery.New(documentquery.Config{Profile: documentquery.LegacyCompatibility,
		Scope: fixture.scope, Snapshots: fixture.snapshots, Legacy: fixture.legacy,
		Index: nil, Verified: nil, RootEvents: nil, AtomicRead: atomicRead})
	if err != nil {
		fixture.test.Fatal(err)
	}
	return queries
}

func (document eventPrefixDocument) page(offset, next, inherited int, positions ...int) domain.DocEventPage {
	events := make([]domain.CIREvent, 0, len(positions))
	for _, position := range positions {
		events = append(events, document.events[position])
	}
	return domain.DocEventPage{Hash: document.document.Hash, Envelope: document.document.CIR.Envelope,
		Events: events, Total: len(document.events), Offset: offset, Next: next, Inherited: inherited}
}

func (document eventPrefixDocument) entry(position int, text string) domain.DocEventIndex {
	offset := 0
	for before := 0; before < position; before++ {
		offset += len(document.json[before]) + 1
	}
	event := document.events[position]
	return domain.DocEventIndex{Index: position, Offset: offset, Length: len(document.json[position]),
		Hash: domain.HashContent([]byte(document.json[position])), Seq: event.Seq, Role: string(event.Role), Text: text}
}

type eventPrefixAPI struct {
	name string
	zero any
	read func(context.Context, *documentquery.Queries, domain.ContentHash, domain.ContentHash) (any, error)
}

func eventPrefixAPIs() []eventPrefixAPI {
	return []eventPrefixAPI{
		{name: "events", zero: domain.DocEventPage{}, read: func(ctx context.Context, queries *documentquery.Queries, repo, hash domain.ContentHash) (any, error) {
			result, err := queries.ReadDocEvents(ctx, repo, hash, "", 0, 2)
			return result, err
		}},
		{name: "fragments", zero: domain.DocFragmentPage{}, read: func(ctx context.Context, queries *documentquery.Queries, repo, hash domain.ContentHash) (any, error) {
			result, err := queries.ReadDocFragments(ctx, repo, hash, 0, 0, 2, 1024)
			return result, err
		}},
		{name: "search", zero: []domain.DocEventIndex(nil), read: func(ctx context.Context, queries *documentquery.Queries, repo, hash domain.ContentHash) (any, error) {
			result, err := queries.SearchDocEvents(ctx, repo, hash, " \tNeEdLe\n ", -1, 2)
			return result, err
		}},
	}
}

func (fixture *eventPrefixFixture) golden(api string) any {
	switch api {
	case "events":
		return fixture.document.page(0, 2, 0, 0, 1)
	case "fragments":
		return domain.DocFragmentPage{Total: len(fixture.document.events), NextIndex: 2,
			Fragments: []domain.DocEventFragment{{Index: 0, Complete: true, JSON: fixture.document.json[0]}, {Index: 1, Complete: true, JSON: fixture.document.json[1]}}}
	case "search":
		return []domain.DocEventIndex{fixture.document.entry(0, "\uc55e\ub9d0 NEEDLE"), fixture.document.entry(1, "\ub450\ubc88\uc9f8 🌿 needle")}
	default:
		fixture.test.Fatalf("unknown event API %q", api)
		return nil
	}
}

func TestEventPrefixLegacyGoldenAPIsAndScopes(test *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, api := range eventPrefixAPIs() {
			test.Run(fmt.Sprintf("%s/chunked=%t", api.name, chunked), func(test *testing.T) {
				fixture := eventPrefix(test)
				fixture.legacy.chunked = chunked
				original := append([]domain.CIREvent{}, fixture.document.document.CIR.Events...)
				result, err := api.read(context.Background(), fixture.queries(true), fixture.repo, fixture.document.document.Hash)
				if err != nil || !reflect.DeepEqual(result, fixture.golden(api.name)) {
					test.Fatalf("legacy golden output changed: got=%+v err=%v", result, err)
				}
				stages := []string{"read", "snapshot", "metadata", "doc"}
				if api.name != "search" {
					stages = append(stages, "manifest")
					if chunked {
						stages = append(stages, "chunk")
					} else {
						stages = append(stages, "doc")
					}
				}
				if !reflect.DeepEqual(fixture.scope.stages(), stages) || fixture.scope.active || fixture.scope.snapshotDepth != 0 {
					test.Fatalf("read/snapshot boundary changed: %+v", fixture.scope.calls)
				}
				for _, call := range fixture.scope.calls {
					if call.repo != fixture.repo || call.stage != "chunk" && call.hash != "" && call.hash != fixture.document.document.Hash {
						test.Fatalf("query changed the exact repository/hash selection: %+v", call)
					}
				}
				stored := fixture.legacy.documents[fixture.document.document.Hash]
				canonical, err := domain.CanonicalBytes(stored.CIR)
				if err != nil || !bytes.Equal(canonical, fixture.document.canonical) || !reflect.DeepEqual(stored.CIR.Events, original) {
					test.Fatal("query mutated the original archive, canonical hash, or sequence input")
				}
			})
		}
	}
}

func TestEventPrefixEmptyArchiveIsSuccessfulAndDoesNotReadPayload(test *testing.T) {
	for _, atomicRead := range []bool{false, true} {
		for _, api := range eventPrefixAPIs() {
			test.Run(fmt.Sprintf("%s/atomic=%t", api.name, atomicRead), func(test *testing.T) {
				fixture := eventPrefixWithJSON(test, nil)
				fixture.legacy.chunked = true
				result, err := api.read(context.Background(), fixture.queries(atomicRead), fixture.repo, fixture.document.document.Hash)
				var want any
				switch api.name {
				case "events":
					want = fixture.document.page(0, -1, 0)
				case "fragments":
					want = domain.DocFragmentPage{Fragments: []domain.DocEventFragment{}}
				case "search":
					want = []domain.DocEventIndex{}
				}
				if err != nil || !reflect.DeepEqual(result, want) || reflect.DeepEqual(result, api.zero) ||
					!reflect.DeepEqual(fixture.scope.stages(), []string{"read", "snapshot", "metadata", "doc"}) {
					test.Fatalf("empty success became an atomic failure or unnecessary payload read: result=%+v err=%v", result, err)
				}
			})
		}
	}
}

func TestEventPrefixMissingManifestFallsBackOnlyToExactVerifiedLegacyDoc(test *testing.T) {
	for _, api := range eventPrefixAPIs()[:2] {
		test.Run(api.name, func(test *testing.T) {
			fixture := eventPrefix(test)
			fixture.scope.before = func(_ context.Context, call eventPrefixCall) error {
				if call.stage == "manifest" {
					return domain.ErrNotFound
				}
				return nil
			}
			result, err := api.read(context.Background(), fixture.queries(true), fixture.repo, fixture.document.document.Hash)
			if err != nil || !reflect.DeepEqual(result, fixture.golden(api.name)) ||
				!reflect.DeepEqual(fixture.scope.stages(), []string{"read", "snapshot", "metadata", "doc", "manifest", "doc"}) {
				test.Fatalf("missing-manifest legacy fallback changed its selected source or scope: %v", err)
			}
		})
	}
}

func TestEventPrefixEventSelectionUsesOnlyExactInheritedPrefix(test *testing.T) {
	fixture := eventPrefix(test)
	fixture.legacy.chunked = true
	prefix := fixture.addDocument(eventPrefixGoldenJSON[:2])
	sparse := fixture.addDocument([]string{eventPrefixGoldenJSON[0], eventPrefixGoldenJSON[2]})
	diverged := fixture.addDocument([]string{eventPrefixGoldenJSON[0], eventPrefixMessageJSON(test, 20, "changed"), eventPrefixGoldenJSON[2]})
	longer := fixture.addDocument(append(append([]string{}, eventPrefixGoldenJSON...), eventPrefixMessageJSON(test, 80, "extra")))
	cases := []struct {
		name      string
		base      domain.ContentHash
		offset    int
		limit     int
		positions []int
		next      int
		inherited int
		selected  int
	}{
		{name: "default limit", limit: 0, positions: []int{0, 1, 2, 3, 4, 5, 6}, next: -1},
		{name: "selected window", offset: 2, limit: 2, positions: []int{2, 3}, next: 4, selected: 2},
		{name: "without base", offset: -1, limit: 100, positions: []int{0, 1, 2, 3, 4, 5, 6}, next: -1},
		{name: "exact eof", offset: 7, limit: 100, next: -1, selected: 7},
		{name: "inherited", base: prefix.document.Hash, offset: -1, limit: 100, positions: []int{2, 3, 4, 5, 6}, next: -1, inherited: 2, selected: 2},
		{name: "explicit offset wins", base: prefix.document.Hash, limit: 1, positions: []int{0}, next: 1, inherited: 2},
		{name: "not a subsequence", base: sparse.document.Hash, offset: -1, limit: 100, positions: []int{1, 2, 3, 4, 5, 6}, next: -1, inherited: 1, selected: 1},
		{name: "same sequence changed hash", base: diverged.document.Hash, offset: -1, limit: 100, positions: []int{1, 2, 3, 4, 5, 6}, next: -1, inherited: 1, selected: 1},
		{name: "same document", base: fixture.document.document.Hash, offset: -1, limit: 100, next: -1, inherited: 7, selected: 7},
		{name: "base longer than target", base: longer.document.Hash, offset: -1, limit: 100, next: -1, inherited: 7, selected: 7},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			fixture.scope.calls = nil
			page, err := fixture.queries(true).ReadDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, scenario.base, scenario.offset, scenario.limit)
			want := fixture.document.page(scenario.selected, scenario.next, scenario.inherited, scenario.positions...)
			if err != nil || !reflect.DeepEqual(page, want) {
				test.Fatalf("exact inherited selection changed: got=%+v want=%+v err=%v", page, want, err)
			}
			var requested []domain.ContentHash
			admissions, snapshots := 0, 0
			for _, call := range fixture.scope.calls {
				switch call.stage {
				case "read":
					admissions++
				case "snapshot":
					snapshots++
				case "doc":
					requested = append(requested, call.hash)
				}
			}
			wantHashes := []domain.ContentHash{fixture.document.document.Hash}
			if scenario.base != "" {
				wantHashes = append([]domain.ContentHash{scenario.base}, wantHashes...)
			}
			if admissions != 1 || snapshots != len(wantHashes) || !reflect.DeepEqual(requested, wantHashes) {
				test.Fatalf("base/target left their single read scope or exact hash ownership: %+v", fixture.scope.calls)
			}
		})
	}
}

func TestEventPrefixFragmentGoldenUTF8AndCursors(test *testing.T) {
	fixture := eventPrefix(test)
	fixture.legacy.chunked = true
	hangul := strings.Index(fixture.document.json[1], "\ub450")
	emoji := strings.Index(fixture.document.json[1], "🌿")
	cases := []struct {
		name       string
		index      int
		offset     int
		limit      int
		budget     int
		fragments  []domain.DocEventFragment
		nextIndex  int
		nextOffset int
	}{
		{name: "event limit", limit: 1, budget: 64 << 10, fragments: []domain.DocEventFragment{{Index: 0, Complete: true, JSON: fixture.document.json[0]}}, nextIndex: 1},
		{name: "event boundary", index: 2, limit: 2, budget: 64 << 10, fragments: []domain.DocEventFragment{{Index: 2, Complete: true, JSON: fixture.document.json[2]}, {Index: 3, Complete: true, JSON: fixture.document.json[3]}}, nextIndex: 4},
		{name: "ascii continuation", offset: 1, limit: 1, budget: 1024, fragments: []domain.DocEventFragment{{Offset: 1, Complete: true, JSON: fixture.document.json[0][1:]}}, nextIndex: 1},
		{name: "hangul budget", index: 1, offset: hangul, limit: 2, budget: 4, fragments: []domain.DocEventFragment{{Index: 1, Offset: hangul, JSON: "\ub450"}}, nextIndex: 1, nextOffset: hangul + len("\ub450")},
		{name: "emoji budget", index: 1, offset: emoji, limit: 2, budget: 4, fragments: []domain.DocEventFragment{{Index: 1, Offset: emoji, JSON: "🌿"}}, nextIndex: 1, nextOffset: emoji + len("🌿")},
		{name: "trim incomplete emoji", index: 1, offset: emoji - 1, limit: 2, budget: 4, fragments: []domain.DocEventFragment{{Index: 1, Offset: emoji - 1, JSON: " "}}, nextIndex: 1, nextOffset: emoji},
		{name: "exact eof", index: len(fixture.document.events), limit: 50, budget: 64 << 10, fragments: []domain.DocEventFragment{}, nextIndex: len(fixture.document.events)},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			page, err := fixture.queries(true).ReadDocFragments(context.Background(), fixture.repo, fixture.document.document.Hash, scenario.index, scenario.offset, scenario.limit, scenario.budget)
			want := domain.DocFragmentPage{Fragments: scenario.fragments, Total: len(fixture.document.events), NextIndex: scenario.nextIndex, NextOffset: scenario.nextOffset}
			if err != nil || !reflect.DeepEqual(page, want) {
				test.Fatalf("UTF-8 fragment golden changed: got=%+v want=%+v err=%v", page, want, err)
			}
		})
	}
}

func eventPrefixMessageJSON(test *testing.T, sequence int, text string) string {
	test.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		test.Fatal(err)
	}
	return `{"blocks":[{"text":` + string(encoded) + `,"type":"text"}],"kind":"message","role":"user","seq":` + strconv.Itoa(sequence) + `}`
}

func eventPrefixPaddedMessage(test *testing.T, sequence, length int) string {
	test.Helper()
	empty := eventPrefixMessageJSON(test, sequence, "")
	if length < len(empty) {
		test.Fatal("invalid padded event fixture extent")
	}
	return eventPrefixMessageJSON(test, sequence, strings.Repeat("x", length-len(empty)))
}

func TestEventPrefixChunkBoundariesAndResponseBudgets(test *testing.T) {
	test.Run("event response cap", func(test *testing.T) {
		fixture := eventPrefixWithJSON(test, []string{eventPrefixPaddedMessage(test, 10, domain.ChunkTarget-10), eventPrefixMessageJSON(test, 20, "\ub05d 🌿")})
		fixture.legacy.chunked = true
		queries := fixture.queries(true)
		first, err := queries.ReadDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, "", 0, 100)
		if err != nil || !reflect.DeepEqual(first, fixture.document.page(0, 1, 0, 0)) {
			test.Fatalf("event response crossed its 512KiB boundary: %v", err)
		}
		fixture.scope.calls = nil
		second, err := queries.ReadDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, "", first.Next, 100)
		if err != nil || !reflect.DeepEqual(second, fixture.document.page(1, -1, 0, 1)) {
			test.Fatalf("event crossing a physical chunk boundary changed: %v", err)
		}
		var chunks []domain.ContentHash
		for _, call := range fixture.scope.calls {
			if call.stage == "chunk" {
				chunks = append(chunks, call.hash)
			}
		}
		if !reflect.DeepEqual(chunks, fixture.document.plan.Manifest.Chunks) || len(chunks) != 2 {
			test.Fatal("boundary read did not authenticate exactly its two selected chunks")
		}
	})
	test.Run("UTF8 split across physical chunks", func(test *testing.T) {
		empty := eventPrefixMessageJSON(test, 10, "")
		textStart := strings.Index(empty, `"text":"`) + len(`"text":"`)
		encoded := eventPrefixMessageJSON(test, 10, strings.Repeat("x", domain.ChunkTarget-1-textStart)+"🌿tail")
		fixture := eventPrefixWithJSON(test, []string{encoded})
		fixture.legacy.chunked = true
		queries := fixture.queries(true)
		page, err := queries.ReadDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, "", 0, 1)
		if err != nil || !reflect.DeepEqual(page, fixture.document.page(0, -1, 0, 0)) || len(encoded) <= 512<<10 {
			test.Fatalf("the first oversized event was truncated or rejected: %v", err)
		}
		for _, boundary := range []struct {
			offset int
			text   string
			next   int
		}{{domain.ChunkTarget - 2, "x", domain.ChunkTarget - 1}, {domain.ChunkTarget - 1, "🌿", domain.ChunkTarget + 3}} {
			fragments, err := queries.ReadDocFragments(context.Background(), fixture.repo, fixture.document.document.Hash, 0, boundary.offset, 1, 4)
			want := domain.DocFragmentPage{Total: 1, NextOffset: boundary.next, Fragments: []domain.DocEventFragment{{Offset: boundary.offset, JSON: boundary.text}}}
			if err != nil || !reflect.DeepEqual(fragments, want) {
				test.Fatalf("physical chunk boundary changed a UTF-8 cursor: %v", err)
			}
		}
		var joined strings.Builder
		index, offset := 0, 0
		for step := 0; step < 32 && index == 0; step++ {
			fixture.scope.calls = nil
			fragments, err := queries.ReadDocFragments(context.Background(), fixture.repo, fixture.document.document.Hash, index, offset, 50, 64<<10)
			if err != nil {
				test.Fatal(err)
			}
			bytesRead, chunks := 0, 0
			for _, fragment := range fragments.Fragments {
				if fragment.Index != 0 || fragment.Offset != joined.Len() || !utf8.ValidString(fragment.JSON) {
					test.Fatal("fragment gap, reordered event, or invalid UTF-8")
				}
				joined.WriteString(fragment.JSON)
				bytesRead += len(fragment.JSON)
			}
			for _, call := range fixture.scope.calls {
				if call.stage == "chunk" {
					chunks++
				}
			}
			if bytesRead == 0 || bytesRead > 64<<10 || chunks > 2 || fragments.Total != 1 || fragments.NextIndex == index && fragments.NextOffset <= offset {
				test.Fatal("fragment budget, two-load bound, or cursor progress changed")
			}
			index, offset = fragments.NextIndex, fragments.NextOffset
		}
		if index != 1 || offset != 0 || joined.String() != encoded || domain.HashContent([]byte(joined.String())) != fixture.document.entry(0, "").Hash {
			test.Fatal("fragment reassembly changed the complete canonical event bytes/hash")
		}
	})
}

func TestEventPrefixSearchGoldenTextAndExclusiveCursor(test *testing.T) {
	fixture := eventPrefix(test)
	texts := []string{"\uc55e\ub9d0 NEEDLE", "\ub450\ubc88\uc9f8 🌿 needle", "", "Needle \uc0dd\uac01", "", "\ub9c8\uc9c0\ub9c9 \uc904\nNEEDLE🙂", ""}
	cases := []struct {
		name      string
		query     string
		after     int
		limit     int
		positions []int
	}{
		{name: "trimmed case insensitive", query: " \tNEEDLE\n", after: -1, limit: 201, positions: []int{0, 1, 3, 5}},
		{name: "exclusive cursor", query: "needle", after: 0, limit: 1, positions: []int{1}},
		{name: "reasoning summary", query: "\uc0dd\uac01", after: -1, limit: 201, positions: []int{3}},
		{name: "multiline", query: "\uc904\nneedle", after: -1, limit: 201, positions: []int{5}},
		{name: "two unicode runes", query: "\uc55e\ub9d0", after: -1, limit: 201, positions: []int{0}},
		{name: "tool fields excluded", query: "tool", after: -1, limit: 201},
		{name: "exhausted", query: "needle", after: 6, limit: 201},
		{name: "256 unicode runes", query: strings.Repeat("\uac00", 256), after: -1, limit: 201},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			fixture.scope.calls = nil
			hits, err := fixture.queries(true).SearchDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, scenario.query, scenario.after, scenario.limit)
			want := make([]domain.DocEventIndex, 0, len(scenario.positions))
			for _, position := range scenario.positions {
				want = append(want, fixture.document.entry(position, texts[position]))
			}
			if err != nil || !reflect.DeepEqual(hits, want) || !reflect.DeepEqual(fixture.scope.stages(), []string{"read", "snapshot", "metadata", "doc"}) {
				test.Fatalf("search changed canonical event offsets, hashes, text, or scope: hits=%+v err=%v", hits, err)
			}
		})
	}
}

func TestEventPrefixPublicValidationBounds(test *testing.T) {
	fixture := eventPrefix(test)
	queries := fixture.queries(true)
	ctx, repo, hash := context.Background(), fixture.repo, fixture.document.document.Hash
	for _, bounds := range [][2]int{{-2, 1}, {0, -1}, {0, 101}, {8, 1}} {
		page, err := queries.ReadDocEvents(ctx, repo, hash, "", bounds[0], bounds[1])
		if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.DocEventPage{}) {
			test.Fatalf("event range %v escaped validation/atomic zero: %v", bounds, err)
		}
	}
	insideRune := strings.Index(fixture.document.json[0], "\uc55e") + 1
	for _, bounds := range [][4]int{{-1, 0, 1, 4}, {0, -1, 1, 4}, {0, 0, 0, 4}, {0, 0, 51, 4}, {0, 0, 1, 3}, {0, 0, 1, (64 << 10) + 1}, {8, 0, 1, 4}, {7, 1, 1, 4}, {0, len(fixture.document.json[0]), 1, 4}, {0, insideRune, 1, 4}} {
		page, err := queries.ReadDocFragments(ctx, repo, hash, bounds[0], bounds[1], bounds[2], bounds[3])
		if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.DocFragmentPage{}) {
			test.Fatalf("fragment range %v escaped validation/atomic zero: %v", bounds, err)
		}
	}
	for _, bounds := range []struct {
		query string
		after int
		limit int
	}{{"", -1, 1}, {" \uac00 ", -1, 1}, {strings.Repeat("\uac00", 257), -1, 1}, {"needle", -2, 1}, {"needle", -1, 0}, {"needle", -1, 202}} {
		hits, err := queries.SearchDocEvents(ctx, repo, hash, bounds.query, bounds.after, bounds.limit)
		if !errors.Is(err, domain.ErrValidation) || hits != nil {
			test.Fatalf("search bounds escaped validation: after=%d limit=%d err=%v", bounds.after, bounds.limit, err)
		}
	}
}

func TestEventPrefixStageErrorsAndCancellationStopAtTheirBoundary(test *testing.T) {
	for _, api := range eventPrefixAPIs() {
		stages := []string{"read", "snapshot", "metadata", "doc"}
		if api.name != "search" {
			stages = append(stages, "manifest", "chunk")
		}
		for boundary, stage := range stages {
			for _, canceled := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/canceled=%t", api.name, stage, canceled), func(test *testing.T) {
					fixture := eventPrefix(test)
					fixture.legacy.chunked = true
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					failure := error(errors.New("event-prefix injected " + stage))
					if canceled {
						failure = context.Canceled
					}
					fixture.scope.before = func(bound context.Context, call eventPrefixCall) error {
						if call.stage != stage {
							return nil
						}
						if canceled {
							cancel()
							return bound.Err()
						}
						return failure
					}
					result, err := api.read(ctx, fixture.queries(true), fixture.repo, fixture.document.document.Hash)
					if !errors.Is(err, failure) || !reflect.DeepEqual(result, api.zero) || !reflect.DeepEqual(fixture.scope.stages(), stages[:boundary+1]) {
						test.Fatalf("error/cancellation crossed a stage or leaked an atomic result: result=%+v err=%v calls=%+v", result, err, fixture.scope.calls)
					}
				})
			}
		}
		for _, atomicRead := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/already canceled/atomic=%t", api.name, atomicRead), func(test *testing.T) {
				fixture := eventPrefix(test)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				result, err := api.read(ctx, fixture.queries(atomicRead), fixture.repo, fixture.document.document.Hash)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, api.zero) || !reflect.DeepEqual(fixture.scope.stages(), []string{"read"}) {
					test.Fatal("canceled admission invoked a document port or manufactured a result")
				}
			})
		}
	}
}

func TestEventPrefixAtomicReadDropsResultsOnlyWhenEnabled(test *testing.T) {
	for _, api := range eventPrefixAPIs() {
		for _, atomicRead := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/atomic=%t", api.name, atomicRead), func(test *testing.T) {
				fixture := eventPrefix(test)
				failure := errors.New("event-prefix snapshot completion failed")
				fixture.scope.afterError = failure
				result, err := api.read(context.Background(), fixture.queries(atomicRead), fixture.repo, fixture.document.document.Hash)
				want := fixture.golden(api.name)
				if atomicRead {
					want = api.zero
				}
				if !errors.Is(err, failure) || !reflect.DeepEqual(result, want) {
					test.Fatalf("AtomicRead changed its zero-result contract: got=%+v want=%+v err=%v", result, want, err)
				}
			})
		}
	}
}

func TestEventPrefixAtomicReadHandlesLateChunkErrorsAndCancellation(test *testing.T) {
	for _, api := range []string{"events", "fragments"} {
		for _, atomicRead := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/atomic=%t/canceled=%t", api, atomicRead, canceled), func(test *testing.T) {
					fixture := eventPrefixWithJSON(test, []string{eventPrefixPaddedMessage(test, 10, domain.ChunkTarget-200), eventPrefixMessageJSON(test, 20, "first selected"), eventPrefixMessageJSON(test, 30, strings.Repeat("needle", 50))})
					fixture.legacy.chunked = true
					if len(fixture.document.plan.Manifest.Chunks) != 2 {
						test.Fatal("late-error fixture must cross exactly two chunks")
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					failure := error(errors.New("event-prefix selected second chunk failed"))
					if canceled {
						failure = context.Canceled
					}
					fixture.scope.before = func(bound context.Context, call eventPrefixCall) error {
						if call.stage != "chunk" || call.hash != fixture.document.plan.Manifest.Chunks[1] {
							return nil
						}
						if canceled {
							cancel()
							return bound.Err()
						}
						return failure
					}
					queries := fixture.queries(atomicRead)
					if api == "events" {
						page, err := queries.ReadDocEvents(ctx, fixture.repo, fixture.document.document.Hash, "", 1, 2)
						want := fixture.document.page(1, -1, 0, 1)
						if atomicRead {
							want = domain.DocEventPage{}
						}
						if !errors.Is(err, failure) || !reflect.DeepEqual(page, want) {
							test.Fatalf("late event failure changed partial/zero output: %v", err)
						}
					} else {
						page, err := queries.ReadDocFragments(ctx, fixture.repo, fixture.document.document.Hash, 1, 0, 2, 1024)
						want := domain.DocFragmentPage{Total: 3, NextIndex: 1, Fragments: []domain.DocEventFragment{{Index: 1, Complete: true, JSON: fixture.document.json[1]}}}
						if atomicRead {
							want = domain.DocFragmentPage{}
						}
						if !errors.Is(err, failure) || !reflect.DeepEqual(page, want) {
							test.Fatalf("late fragment failure changed partial/zero output: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestEventPrefixCanonicalHashSnapshotAndOwnershipFences(test *testing.T) {
	for _, api := range eventPrefixAPIs() {
		for _, damage := range []string{"foreign repository", "unknown exact hash", "snapshot id", "snapshot doc hash", "canonical bytes"} {
			test.Run(api.name+"/"+damage, func(test *testing.T) {
				fixture := eventPrefix(test)
				repo, hash := fixture.repo, fixture.document.document.Hash
				wantError := domain.ErrIntegrity
				switch damage {
				case "foreign repository":
					repo = domain.HashContent([]byte("event-prefix-foreign-repository"))
					wantError = domain.ErrNotFound
				case "unknown exact hash":
					hash = domain.HashContent([]byte("event-prefix-unowned-document"))
					wantError = domain.ErrNotFound
				case "snapshot id", "snapshot doc hash":
					snapshot := fixture.snapshots.snapshots[hash]
					other := domain.HashContent([]byte("event-prefix-other-document"))
					if damage == "snapshot id" {
						snapshot.ID = other
					} else {
						snapshot.DocHash = other
					}
					fixture.snapshots.snapshots[hash] = snapshot
				case "canonical bytes":
					document := fixture.legacy.documents[hash]
					document.CIR.Events = append([]domain.CIREvent{}, document.CIR.Events...)
					for position, event := range document.CIR.Events {
						if event.Kind == domain.EventMessage {
							event.Blocks = append([]domain.ContentBlock{}, event.Blocks...)
							event.Blocks[0].Text += " tampered"
							document.CIR.Events[position] = event
							break
						}
					}
					fixture.legacy.documents[hash] = document
				}
				result, err := api.read(context.Background(), fixture.queries(true), repo, hash)
				if !errors.Is(err, wantError) || !reflect.DeepEqual(result, api.zero) {
					test.Fatalf("canonical/source ownership fence was weakened: result=%+v err=%v", result, err)
				}
				for _, call := range fixture.scope.calls {
					if call.repo != repo || call.hash != "" && call.hash != hash || call.stage == "manifest" || call.stage == "chunk" {
						test.Fatalf("rejected source selected another hash or read payload: %+v", call)
					}
				}
			})
		}
		test.Run(api.name+"/legacy snapshot absent", func(test *testing.T) {
			fixture := eventPrefix(test)
			delete(fixture.snapshots.snapshots, fixture.document.document.Hash)
			result, err := api.read(context.Background(), fixture.queries(true), fixture.repo, fixture.document.document.Hash)
			if err != nil || !reflect.DeepEqual(result, fixture.golden(api.name)) {
				test.Fatalf("legacy missing-snapshot compatibility changed: %v", err)
			}
		})
	}
	test.Run("base ownership checked before target", func(test *testing.T) {
		fixture := eventPrefix(test)
		base := domain.HashContent([]byte("event-prefix-unowned-base"))
		page, err := fixture.queries(true).ReadDocEvents(context.Background(), fixture.repo, fixture.document.document.Hash, base, -1, 100)
		if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(page, domain.DocEventPage{}) {
			test.Fatal("an unowned base was treated as an empty inherited prefix")
		}
		for _, call := range fixture.scope.calls {
			if call.hash != "" && call.hash != base {
				test.Fatalf("target payload was read after base ownership failed: %+v", call)
			}
		}
	})
}

func TestEventPrefixSelectedChunksKeepTheirHashAndFormatFences(test *testing.T) {
	for _, api := range eventPrefixAPIs()[:2] {
		for _, damage := range []string{"chunk hash", "chunk short", "v1 manifest", "unowned chunk"} {
			test.Run(api.name+"/"+damage, func(test *testing.T) {
				fixture := eventPrefix(test)
				fixture.legacy.chunked = true
				hash := fixture.document.document.Hash
				manifest := fixture.legacy.manifests[hash]
				manifest.Chunks = append([]domain.ContentHash{}, manifest.Chunks...)
				chunkHash := manifest.Chunks[0]
				body := bytes.Clone(fixture.legacy.chunks[chunkHash])
				wantError := domain.ErrIntegrity
				switch damage {
				case "chunk hash":
					body[0] ^= 1
					fixture.legacy.chunks[chunkHash] = body
				case "chunk short":
					fixture.legacy.chunks[chunkHash] = body[:len(body)-1]
				case "v1 manifest":
					manifest.Format = domain.ChunkFormatV1
				case "unowned chunk":
					manifest.Chunks[0] = domain.HashContent([]byte("event-prefix-unowned-chunk"))
					wantError = domain.ErrNotFound
				}
				fixture.legacy.manifests[hash] = manifest
				result, err := api.read(context.Background(), fixture.queries(true), fixture.repo, hash)
				if !errors.Is(err, wantError) || !reflect.DeepEqual(result, api.zero) {
					test.Fatalf("selected chunk corruption/ownership became a successful read: %v", err)
				}
			})
		}
	}
}
