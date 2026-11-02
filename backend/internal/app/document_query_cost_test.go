package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentQueryCostRequest struct {
	mode string
	page string
	want domain.DocViewPage
	wire []byte
}

type documentQueryCostFixture struct {
	name     string
	turns    int
	doc      domain.SessionDoc
	manifest domain.ConversationManifest
	reader   *docViewCountStore
	service  *Service
	requests []documentQueryCostRequest
}

type documentQueryCostSelection struct {
	digest   domain.ContentHash
	offset   int
	next     int
	events   int
	inline   int
	visible  int
	chunks   int
	pages    int
	wholeDoc int
}

func TestDocumentQueryPreparedRootCost(t *testing.T) {
	const limit = 8
	const wireLimit = 1 << 20
	pageLimit := 1 + (docViewScanEvents+domain.DocRangePageEvents-2)/domain.DocRangePageEvents
	modes := []string{"chat", "prompts", "all"}
	documentQueryCostLog(t, map[string]any{
		"kind":               "contract",
		"card":               "C23",
		"fixture_input_tree": "340788a06db9b79c52cd200e662e780ffe820399",
		"executing_source":   "bound by the parent source manifest and run log, not the fixture input tree",
		"baseline_tree":      "9d2f780f07d1ad912948c04415e603c170d2c304",
		"fixture_status":     "temporary deterministic synthetic sample; no observed production distribution; not giant/tool-heavy representative evidence",
		"envelope":           historyEnvelope(),
		"turns":              []int{24, 240},
		"messages":           "each turn: user turn-%03d + question repeated 128 times; assistant turn-%03d + answer repeated 256 times",
		"tools":              "turn%4==0: one lookup call with a string query and one short string result, between user and assistant",
		"modes":              modes,
		"limit":              limit,
		"requests":           "base empty; first offset 0; next offset from first.Next; two sequential pages per mode, no parallelism or hidden repeats",
		"measurement":        "seed, EnsureDocRangeIndex, expectations and log encoding outside timers; read_ns covers only public Service.ReadDocViewEvents; encode_ns covers only json.Marshal(page)",
		"metrics":            []string{"read_ns", "encode_ns", "chunk_reads", "index_page_reads", "whole_doc_reads", "offset", "end_exclusive", "next", "events", "visible", "inline", "inline_event_bytes", "wire_bytes", "result_digest", "selection_digest"},
		"counter_scope":      "docViewCountStore ReadConversationChunk/DocRangePage/ReadVerifiedDoc calls, not disk IO/SQL or all inherited FS methods; internal FS root/header reads and preparation internals are not counted; chunk byte size can differ within fixed chunk geometry",
		"wire_scope":         "json.Marshal domain.DocViewPage bytes, not an HTTP envelope or PG response",
		"digest_scope":       "result_digest hashes exact page JSON; selection_digest hashes only Events for shared-prefix comparison; neither includes timings, temporary paths or randomness",
		"existing_bounds": map[string]any{
			"scan_events":                  docViewScanEvents,
			"index_page_reads":             pageLimit,
			"chunk_reads_per_inline_event": 2,
			"whole_doc_reads":              0,
			"wire_bytes":                   wireLimit,
			"basis":                        "doc_view_corpus_test.go:TestDocViewLocalCorpus; doc_view.go/docViewScanEvents; domain.DocRangePageEvents",
		},
		"fixture_bounds": "same prefix stays in one index page and one chunk: exactly 1 page, 1 chunk, 0 whole-doc reads; basis doc_view_test.go:TestDocViewPageReadsDoNotGrowWithArchive",
		"termination":    "read bounds, exact expected wire/content and shared-prefix equality; elapsed observations have no performance percentage or speedup gate",
		"limits":         "prepared FS only; no cold-cache reset, allocations/RSS, actual HTTP/PG/transaction verification or production latency inference; parent runs identical source on baseline/candidate in three alternating repetitions",
	})

	ctx, repo := systemTestContext(), h('1')
	fixtures := []documentQueryCostFixture{{name: "small", turns: 24}, {name: "medium", turns: 240}}
	for index := range fixtures {
		fixture := &fixtures[index]
		directory := t.TempDir()
		storage := store.NewFSStore(directory)
		fixture.doc, fixture.manifest = seedRootReadDoc(t, directory, storage, repo, documentQueryCostEvents(fixture.turns)...)
		if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.doc.Hash); err != nil {
			t.Fatal("prepare root range index", err)
		}
		fixture.reader = &docViewCountStore{FSStore: storage}
		fixture.service = NewService(storage, fixture.reader, nil, nil, nil)
		documentQueryCostLog(t, map[string]any{
			"kind": "fixture", "fixture": fixture.name, "turns": fixture.turns,
			"events": len(fixture.doc.CIR.Events), "root_digest": fixture.doc.Hash,
			"stream_bytes": fixture.manifest.StreamBytes, "chunks": len(fixture.manifest.Chunks),
			"index_pages": (len(fixture.doc.CIR.Events) + domain.DocRangePageEvents - 1) / domain.DocRangePageEvents,
		})
		for _, mode := range modes {
			offset := 0
			for _, pageName := range []string{"first", "next"} {
				want := documentQueryCostExpectedPage(t, fixture.doc, mode, offset, limit)
				wire, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if want.Next < 0 || want.Next >= domain.DocRangePageEvents || len(wire) > docViewBytes {
					t.Fatal("fixture must provide two small, nonterminal pages in the shared first index page")
				}
				fixture.requests = append(fixture.requests, documentQueryCostRequest{mode: mode, page: pageName, want: want, wire: wire})
				documentQueryCostLog(t, map[string]any{
					"kind": "request", "fixture": fixture.name, "mode": mode, "page": pageName,
					"offset": offset, "limit": limit, "end_exclusive": offset + len(want.Events),
					"next": want.Next, "expected_result_digest": domain.HashContent(wire),
				})
				offset = want.Next
			}
		}
	}
	if len(fixtures[0].manifest.Chunks) != 1 || len(fixtures[1].manifest.Chunks) <= len(fixtures[0].manifest.Chunks) || fixtures[1].manifest.StreamBytes <= fixtures[0].manifest.StreamBytes {
		t.Fatal("fixture must grow from one chunk to multiple chunks without growing the requested prefix")
	}
	sharedPrefix := map[string]documentQueryCostSelection{}
	for _, fixture := range fixtures {
		offset := 0
		for _, request := range fixture.requests {
			key := request.mode + "/" + request.page
			t.Run(fixture.name+"/"+key, func(t *testing.T) {
				if request.page == "first" {
					offset = 0
				}
				if offset != request.want.Offset {
					t.Fatalf("continuation changed: got %d want %d", offset, request.want.Offset)
				}
				reader := fixture.reader
				reader.chunkReads, reader.pageReads, reader.fullReads = 0, 0, 0
				started := time.Now()
				page, err := fixture.service.ReadDocViewEvents(ctx, repo, fixture.doc.Hash, "", offset, limit, request.mode)
				readElapsed := time.Since(started)
				if err != nil {
					t.Fatal("prepared public read", err)
				}
				started = time.Now()
				wire, err := json.Marshal(page)
				encodeElapsed := time.Since(started)
				if err != nil {
					t.Fatal("encode response", err)
				}
				selectionWire, err := json.Marshal(page.Events)
				if err != nil {
					t.Fatal(err)
				}
				inline, visible, inlineBytes := 0, 0, 0
				for _, event := range page.Events {
					if !event.Deferred {
						inline++
						inlineBytes += event.Bytes
					}
					if request.mode == "all" || event.Event.Kind == domain.EventMessage && (request.mode == "chat" || event.Event.Role == domain.RoleUser) {
						visible++
					}
				}
				observation := documentQueryCostSelection{
					digest: domain.HashContent(selectionWire), offset: page.Offset, next: page.Next,
					events: len(page.Events), inline: inline, visible: visible,
					chunks: reader.chunkReads, pages: reader.pageReads, wholeDoc: reader.fullReads,
				}
				documentQueryCostLog(t, map[string]any{
					"kind": "result", "fixture": fixture.name, "turns": fixture.turns, "mode": request.mode, "page": request.page,
					"requested_offset": offset, "limit": limit, "offset": page.Offset, "end_exclusive": page.Offset + len(page.Events), "next": page.Next, "total": page.Total,
					"read_ns": readElapsed.Nanoseconds(), "encode_ns": encodeElapsed.Nanoseconds(),
					"chunk_reads": reader.chunkReads, "index_page_reads": reader.pageReads, "whole_doc_reads": reader.fullReads,
					"events": len(page.Events), "visible": visible, "inline": inline, "inline_event_bytes": inlineBytes,
					"wire_bytes": len(wire), "result_digest": domain.HashContent(wire), "selection_digest": observation.digest,
					"expected_result_digest": domain.HashContent(request.wire), "content_equal": bytes.Equal(wire, request.wire),
				})
				if !bytes.Equal(wire, request.wire) {
					t.Error("page differs from deterministic source expectation, including deferred metadata and continuation")
				}
				if reader.fullReads != 0 || reader.chunkReads > 2*inline || reader.pageReads > pageLimit || len(page.Events) > docViewScanEvents || visible != limit || len(wire) > wireLimit || page.Next <= offset {
					t.Error("existing range-read bounds or continuation regressed")
				}
				if reader.chunkReads != 1 || reader.pageReads != 1 {
					t.Error("small prefix must read exactly one chunk and one index page regardless of total document size")
				}
				if fixture.name == "small" {
					sharedPrefix[key] = observation
				} else {
					previous, found := sharedPrefix[key]
					equal := found && previous == observation
					documentQueryCostLog(t, map[string]any{
						"kind": "comparison", "mode": request.mode, "page": request.page,
						"small_selection_digest": previous.digest, "medium_selection_digest": observation.digest,
						"same_selection_and_read_counts": equal,
					})
					if !equal {
						t.Error("shared selection/content or read counts changed with document size")
					}
				}
				offset = page.Next
			})
		}
	}
}

func documentQueryCostEvents(turns int) []domain.CIREvent {
	events := make([]domain.CIREvent, 0, turns*3)
	for turn := 0; turn < turns; turn++ {
		label := fmt.Sprintf("turn-%03d ", turn)
		events = append(events, historyMessage(domain.RoleUser, label+strings.Repeat("question ", 128)))
		if turn%4 == 0 {
			callID := fmt.Sprintf("lookup-%03d", turn)
			events = append(events,
				domain.CIREvent{Kind: domain.EventToolCall, CallID: callID, ToolName: "lookup", Input: map[string]any{"query": label + "symbol"}},
				domain.CIREvent{Kind: domain.EventToolResult, CallID: callID, Output: label + "one matching line"},
			)
		}
		events = append(events, historyMessage(domain.RoleAssistant, label+strings.Repeat("answer ", 256)))
	}
	return events
}

func documentQueryCostExpectedPage(t *testing.T, doc domain.SessionDoc, mode string, offset, limit int) domain.DocViewPage {
	t.Helper()
	page := domain.DocViewPage{Hash: doc.Hash, DocIdentity: domain.DocumentIdentityRootV1, Envelope: doc.CIR.Envelope, Total: len(doc.CIR.Events), Offset: offset, Next: -1, Events: []domain.DocViewEvent{}}
	visible := 0
	for position := offset; position < len(doc.CIR.Events) && visible < limit; position++ {
		event := doc.CIR.Events[position]
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > docViewBytes {
			t.Fatal("ordinary fixture unexpectedly contains an oversized event")
		}
		shown := mode == "all" || event.Kind == domain.EventMessage && (mode == "chat" || event.Role == domain.RoleUser)
		deferred := event.Kind != domain.EventMessage || !shown
		if deferred {
			event = domain.CIREvent{Kind: event.Kind, Role: event.Role, Seq: event.Seq, ToolName: event.ToolName}
		}
		page.Events = append(page.Events, domain.DocViewEvent{Event: event, Index: position, Deferred: deferred, Bytes: len(raw)})
		if shown {
			visible++
		}
	}
	if end := offset + len(page.Events); end < len(doc.CIR.Events) {
		page.Next = end
	}
	return page
}

func documentQueryCostLog(t *testing.T, record any) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("document_query_cost_v1 %s", raw)
}
