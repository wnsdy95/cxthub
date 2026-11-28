package documentquery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
	// authorized counts how often a read asked whether its caller may read;
	// refusal, when set, is the answer it gets.
	authorized int
	refusal    error
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
	scope.authorized++
	if scope.refusal != nil {
		return ctx, scope.refusal
	}
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

// WithinDocumentSnapshot is entered inside a document read, or on its own by
// the paged reads, which are admitted before they come here. On its own it is
// the read scope of the document ports, as in the server's scope.
func (scope *eventPrefixLocalScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope.active {
		scope.requireBound(ctx, repo)
	}
	if err := scope.record(ctx, "snapshot", repo, ""); err != nil {
		return err
	}
	if !scope.active {
		scope.repo, scope.active = repo, true
		defer func() { scope.active = false }()
		ctx = context.WithValue(ctx, eventPrefixScopeKey{}, scope)
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

type eventPrefixDocument struct {
	document  domain.SessionDoc
	canonical []byte
	json      []string
	events    []domain.CIREvent
	plan      domain.DocChunkPlan
}

type eventPrefixFixture struct {
	test     *testing.T
	repo     domain.ContentHash
	scope    *eventPrefixLocalScope
	legacy   *eventPrefixLegacy
	document eventPrefixDocument
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
			manifests: make(map[domain.ContentHash]domain.DocChunkManifest), chunks: make(map[domain.ContentHash][]byte)}}
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
		Scope: fixture.scope, Legacy: fixture.legacy,
		Index: nil, AtomicRead: atomicRead})
	if err != nil {
		fixture.test.Fatal(err)
	}
	return queries
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
			if err != nil || !reflect.DeepEqual(hits, want) || !reflect.DeepEqual(fixture.scope.stages(), []string{"read", "doc"}) {
				test.Fatalf("search changed canonical event offsets, hashes, text, or scope: hits=%+v err=%v", hits, err)
			}
		})
	}
}
