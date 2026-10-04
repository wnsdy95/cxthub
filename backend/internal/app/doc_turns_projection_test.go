package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func historyProjectionRequest() domain.AgentHistoryPageRequest {
	return domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20, IncompleteTail: "omit"}
}

func TestHistoryProjectionOmitsOnlyWholeFinalTurnAndKeepsOriginal(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, marker := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marker=%t", provider, marker), func(t *testing.T) {
				ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
				env := historyEnvelope()
				env.SourceProvider = provider
				events := []domain.CIREvent{historyMessage(domain.RoleUser, "first"), historyMessage(domain.RoleAssistant, "first answer"), historyMessage(domain.RoleUser, "second"), historyMessage(domain.RoleAssistant, "second answer")}
				if marker {
					events = append(events, domain.CIREvent{Kind: domain.EventTurn, Role: domain.RoleUser})
				}
				events = append(events, historyMessage(domain.RoleUser, "entire last question"),
					domain.CIREvent{Kind: domain.EventToolCall, CallID: "done", ToolName: "read"},
					domain.CIREvent{Kind: domain.EventToolResult, CallID: "done", Output: "finished"},
					domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"},
					historyMessage(domain.RoleAssistant, "still part of the unfinished turn"))
				doc := putHistoryDoc(t, st, repo, env, events...)
				original, _ := domain.CanonicalBytes(doc.CIR)
				svc := NewService(st, st, nil, nil, nil)
				req := historyProjectionRequest()
				req.IncompleteTail = ""
				if _, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatalf("legacy strict request accepted an unfinished tail: %v", err)
				}
				req.IncompleteTail, req.Limit = "omit", 1
				for _, before := range []int{-1, len(events)} {
					req.Before = before
					page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
					wantTail := &domain.AgentHistoryTail{Start: 4, End: len(events), Reason: "incomplete_tool_pair"}
					if err != nil || page.Version != domain.AgentHistoryProjectionVersion || page.Hash != doc.Hash || page.Total != len(events) || page.Before != len(events) || page.Covered || !reflect.DeepEqual(page.OmittedTail, wantTail) || len(page.Turns) != 1 || page.NextBefore != 2 {
						t.Fatalf("bad whole-tail projection: %+v %v", page, err)
					}
					turn := page.Turns[0]
					wire, _ := json.Marshal(doc.CIR.Events[2:4])
					if turn.Start != 2 || turn.End != 4 || turn.Hash != domain.HashContent(wire) || !reflect.DeepEqual(turn.Events, doc.CIR.Events[2:4]) {
						t.Fatal("changed the preceding complete turn or its wire hash")
					}
				}
				req.Before = 2
				page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
				if err != nil || page.Version != domain.AgentHistoryProjectionVersion || page.Before != 2 || page.OmittedTail != nil || page.NextBefore != -1 || len(page.Turns) != 1 || !reflect.DeepEqual(page.Turns[0].Events, doc.CIR.Events[:2]) {
					t.Fatalf("continuation changed source ordinals: %+v %v", page, err)
				}
				stored, err := st.GetDoc(ctx, repo, doc.Hash)
				after, _ := domain.CanonicalBytes(stored.CIR)
				if err != nil || stored.Hash != doc.Hash || string(original) != string(after) {
					t.Fatal("projection changed the stored CIR", err)
				}
			})
		}
	}
}

func TestHistoryProjectionRejectsMalformedTailEvenWithOutstandingCalls(t *testing.T) {
	open := domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"}
	result := domain.CIREvent{Kind: domain.EventToolResult, CallID: "open", Output: "done"}
	for _, tc := range []struct {
		name string
		bad  []domain.CIREvent
	}{
		{"duplicate call", []domain.CIREvent{open}},
		{"missing call id", []domain.CIREvent{{Kind: domain.EventToolCall, ToolName: "read"}}},
		{"orphan result", []domain.CIREvent{{Kind: domain.EventToolResult, CallID: "other", Output: "orphan"}}},
		{"missing result id", []domain.CIREvent{{Kind: domain.EventToolResult, Output: "orphan"}}},
		{"duplicate result", []domain.CIREvent{result, result}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			events := append([]domain.CIREvent{historyMessage(domain.RoleUser, "keep"), historyMessage(domain.RoleAssistant, "answer"), historyMessage(domain.RoleUser, "bad tail"), open}, tc.bad...)
			// A remaining open call must not mask errors anywhere before it.
			events = append(events, domain.CIREvent{Kind: domain.EventToolCall, CallID: "also-open", ToolName: "read"})
			doc := putHistoryDoc(t, st, repo, historyEnvelope(), events...)
			if _, err := NewService(st, st, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, historyProjectionRequest()); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("malformed tail became an omission: %v", err)
			}
		})
	}
}

func TestHistoryProjectionClassifiesUserBoundariesBeforePendingCalls(t *testing.T) {
	for _, events := range [][]domain.CIREvent{
		{{Kind: domain.EventToolCall, Role: domain.RoleUser, CallID: "invalid-role", ToolName: "read"}},
		{historyMessage(domain.RoleUser, "first"), historyMessage(domain.RoleUser, "second")},
		{{Kind: domain.EventTurn, Role: domain.RoleUser}, {Kind: domain.EventTurn, Role: domain.RoleUser}},
	} {
		events = append(events, domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
		for i := range events {
			events[i].Seq = i
		}
		if incomplete, err := classifyHistoryTurn(events, true); incomplete || !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("invalid user boundary classified as pending only: incomplete=%t err=%v", incomplete, err)
		}
	}
}

func TestHistoryProjectionPendingCallDoesNotHideMalformedSequenceOrKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		last domain.CIREvent
	}{
		{"negative", domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: -1}},
		{"duplicate", domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 4}},
		{"descending", domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 3}},
		{"unknown kind", domain.CIREvent{Kind: "unknown", Seq: 5}},
		{"invalid union", domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 5, ReplacementComplete: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []domain.CIREvent{
				{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 2},
				{Kind: domain.EventToolCall, CallID: "open", ToolName: "read", Seq: 4},
				tc.last,
			}
			if incomplete, err := classifyHistoryTurn(events, true); incomplete || !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("malformed omitted event accepted: incomplete=%t err=%v", incomplete, err)
			}
			if incomplete, err := classifyHistoryTurn(events, false); !incomplete || err != nil {
				t.Fatalf("legacy classifier contract changed: incomplete=%t err=%v", incomplete, err)
			}
		})
	}
	events := []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 2},
		{Kind: domain.EventToolCall, CallID: "open", ToolName: "read", Seq: 4},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 7},
	}
	if incomplete, err := classifyHistoryTurn(events, true); !incomplete || err != nil {
		t.Fatalf("valid noncontiguous ascending sequence rejected: incomplete=%t err=%v", incomplete, err)
	}
}

func TestHistoryProjectionIncludesLargeMarkerOrFailsBounded(t *testing.T) {
	for _, markerBytes := range []int{2048, 4 << 20} {
		t.Run(fmt.Sprint(markerBytes), func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			doc := putHistoryDoc(t, st, repo, historyEnvelope(),
				domain.CIREvent{Kind: domain.EventTurn, Role: domain.RoleUser, ID: strings.Repeat("m", markerBytes)},
				historyMessage(domain.RoleUser, "tail"),
				domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
			spy := &historyReadSpy{FSStore: st}
			page, err := NewService(st, spy, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, historyProjectionRequest())
			if markerBytes == 4<<20 {
				if !errors.Is(err, domain.ErrContextBudgetExceeded) || page.OmittedTail != nil || spy.chunks != 0 {
					t.Fatalf("unclassified oversized marker was excluded from tail: tail=%+v chunks=%d err=%v", page.OmittedTail, spy.chunks, err)
				}
			} else if err != nil || page.OmittedTail == nil || page.OmittedTail.Start != 0 || page.OmittedTail.End != 3 || len(page.Turns) != 0 || page.NextBefore != -1 {
				t.Fatalf("marker split from omitted turn: %+v %v", page, err)
			}
		})
	}
}

func TestHistoryProjectionRejectsStoredDuplicateSequenceWithoutChangingV1(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: historyEnvelope(), Events: []domain.CIREvent{
				{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 0},
				{Kind: domain.EventToolCall, CallID: "open", ToolName: "read", Seq: 1},
				{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 1},
			}}}
			if complete {
				doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventToolResult, CallID: "open", Output: "done", Seq: 2})
			}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			doc.Hash = domain.HashContent(raw)
			if _, err := st.PutDoc(ctx, repo, doc); err != nil {
				t.Fatal(err)
			}
			svc, req := NewService(st, st, nil, nil, nil), historyProjectionRequest()
			if page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) || page.OmittedTail != nil || !strings.Contains(err.Error(), "sequence") {
				t.Fatalf("invalid sequence escaped v2 validation: tail=%+v err=%v", page.OmittedTail, err)
			}
			req.IncompleteTail = ""
			page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
			if complete && (err != nil || page.Version != domain.AgentHistoryPageVersion || len(page.Turns) != 1) || !complete && !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("legacy validation changed: turns=%d err=%v", len(page.Turns), err)
			}
		})
	}
}

func TestHistoryProjectionReturnBudgetIsExactWireBytes(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			events := []domain.CIREvent{historyMessage(domain.RoleUser, "first"), historyMessage(domain.RoleAssistant, "first answer"), historyMessage(domain.RoleUser, strings.Repeat("<>&", 20)), historyMessage(domain.RoleAssistant, "last complete answer")}
			if incomplete {
				events = append(events, historyMessage(domain.RoleUser, "tail"), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
			}
			doc := putHistoryDoc(t, st, repo, historyEnvelope(), events...)
			wire, _ := json.Marshal(doc.CIR.Events[2:4])
			svc, req := NewService(st, st, nil, nil, nil), historyProjectionRequest()
			req.MaxBytes = len(wire)
			page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
			if err != nil || len(page.Turns) != 1 || page.Turns[0].Hash != domain.HashContent(wire) || page.NextBefore != 2 || (page.OmittedTail != nil) != incomplete {
				t.Fatalf("exact wire limit rejected or exceeded: %+v err=%v", page, err)
			}
			req.MaxBytes--
			page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
			if incomplete {
				if err != nil || page.OmittedTail == nil || len(page.Turns) != 0 || page.NextBefore != 4 {
					t.Fatalf("return budget exceeded after omission: %+v err=%v", page, err)
				}
			} else if !errors.Is(err, domain.ErrContextBudgetExceeded) || page.OmittedTail != nil {
				t.Fatalf("oversized complete turn omitted: %+v err=%v", page, err)
			}
		})
	}
}

func TestHistoryProjectionValidatesGlobalIndexSequenceBeforeBodiesOrProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  [4]int
	}{
		{"duplicate across user boundary", [4]int{0, 1, 1, 2}},
		{"negative before otherwise valid tail", [4]int{-1, 0, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: historyEnvelope(), Events: []domain.CIREvent{
				historyMessage(domain.RoleUser, "older"), historyMessage(domain.RoleAssistant, "answer"),
				historyMessage(domain.RoleUser, "tail"), {Kind: domain.EventToolCall, CallID: "open", ToolName: "read"},
			}}}
			for i := range doc.CIR.Events {
				doc.CIR.Events[i].Seq = tc.seq[i]
			}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			doc.Hash = domain.HashContent(raw)
			if _, err := st.PutDoc(ctx, repo, doc); err != nil {
				t.Fatal(err)
			}
			spy := &historyReadSpy{FSStore: st}
			svc := NewService(st, spy, nil, nil, nil)
			for _, before := range []int{-1, 0, 2} {
				for _, coveredBy := range []domain.ContentHash{"", doc.Hash} {
					req := historyProjectionRequest()
					req.Before, req.CoveredBy = before, coveredBy
					page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
					if !errors.Is(err, domain.ErrIntegrity) || page.OmittedTail != nil || page.Covered || spy.chunks != 0 || spy.whole != 0 {
						t.Fatalf("bad global sequence bypassed validation: before=%d proof=%t tail=%+v covered=%t chunks=%d err=%v", before, coveredBy != "", page.OmittedTail, page.Covered, spy.chunks, err)
					}
					// Legacy metadata/proofs and the earlier complete turn retain
					// their old semantics. An uncovered tail remains a pair error.
					req.IncompleteTail = ""
					page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
					if before == -1 && coveredBy == "" {
						if !errors.Is(err, domain.ErrIntegrity) {
							t.Fatal("legacy tail no longer fails strictly", err)
						}
					} else if err != nil || page.Version != domain.AgentHistoryPageVersion || page.Covered != (coveredBy != "") {
						t.Fatalf("legacy metadata/proof/page changed: before=%d proof=%t page=%+v err=%v", before, coveredBy != "", page, err)
					}
					spy.chunks = 0
				}
			}
			if tc.seq[0] == 0 {
				// A valid source does not make a malformed proof candidate valid.
				older := putHistoryDoc(t, st, repo, historyEnvelope(), doc.CIR.Events[:2]...)
				req := historyProjectionRequest()
				req.Before, req.CoveredBy = 0, doc.Hash
				page, err := svc.ReadAgentHistoryPage(ctx, repo, older.Hash, req)
				if !errors.Is(err, domain.ErrIntegrity) || page.Covered || spy.chunks != 0 || spy.whole != 0 {
					t.Fatalf("malformed covered_by index authorized a proof: covered=%t chunks=%d err=%v", page.Covered, spy.chunks, err)
				}
				req.IncompleteTail = ""
				if page, err := svc.ReadAgentHistoryPage(ctx, repo, older.Hash, req); err != nil || !page.Covered {
					t.Fatal("legacy prefix proof changed", err)
				}
			}
		})
	}
}

func TestHistoryProjectionNeverOmitsIncompleteInteriorOrLaterPage(t *testing.T) {
	ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "incomplete interior"), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"}, historyMessage(domain.RoleUser, "complete final"), historyMessage(domain.RoleAssistant, "answer"))
	svc, req := NewService(st, st, nil, nil, nil), historyProjectionRequest()
	if _, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("interior incompleteness was omitted: %v", err)
	}
	req.Limit = 1
	page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || page.OmittedTail != nil || page.NextBefore != 2 || len(page.Turns) != 1 {
		t.Fatalf("complete final turn changed: %+v %v", page, err)
	}
	req.Before = page.NextBefore
	if _, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("later-page last candidate was treated as the document tail: %v", err)
	}
}

func TestHistoryProjectionEmptyTailAndIndependentReturnByteLimit(t *testing.T) {
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprintf("older=%t", older), func(t *testing.T) {
			ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
			var events []domain.CIREvent
			if older {
				events = append(events, historyMessage(domain.RoleUser, "older"), historyMessage(domain.RoleAssistant, "answer"))
			}
			start := len(events)
			events = append(events, domain.CIREvent{Kind: domain.EventTurn, Role: domain.RoleUser}, historyMessage(domain.RoleUser, "final"), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
			doc := putHistoryDoc(t, st, repo, historyEnvelope(), events...)
			svc, req := NewService(st, st, nil, nil, nil), historyProjectionRequest()
			req.MaxBytes = 1 // Omitted body classification has a separate allowance.
			page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
			next := -1
			if older {
				next = start
			}
			if err != nil || len(page.Turns) != 0 || page.Turns == nil || page.OmittedTail == nil || page.OmittedTail.Start != start || page.OmittedTail.End != len(events) || page.NextBefore != next {
				t.Fatalf("bad gap-only page: %+v %v", page, err)
			}
			if older {
				req.Before, req.MaxBytes = page.NextBefore, 4<<20
				page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
				if err != nil || page.OmittedTail != nil || len(page.Turns) != 1 || page.NextBefore != -1 || !reflect.DeepEqual(page.Turns[0].Events, doc.CIR.Events[:start]) {
					t.Fatalf("gap-only cursor did not make progress: %+v %v", page, err)
				}
			}
		})
	}
}

func TestHistoryProjectionBoundsCumulativeClassificationReads(t *testing.T) {
	ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, strings.Repeat("o", 3<<19)), historyMessage(domain.RoleAssistant, "old answer"), historyMessage(domain.RoleUser, strings.Repeat("t", 3<<20)), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
	man, err := st.GetDocManifest(ctx, repo, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	spy := &historyReadSpy{FSStore: st, forbidden: man.Chunks[0]}
	svc, req := NewService(st, spy, nil, nil, nil), historyProjectionRequest()
	page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || len(page.Turns) != 0 || page.OmittedTail == nil || page.OmittedTail.Start != 2 || page.NextBefore != 2 || spy.whole != 0 || spy.chunks > 8 {
		t.Fatalf("classification exceeded shared allowance: turns=%d tail=%+v next=%d chunks=%d err=%v", len(page.Turns), page.OmittedTail, page.NextBefore, spy.chunks, err)
	}
	spy.forbidden = ""
	req.Before = page.NextBefore
	page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || page.OmittedTail != nil || len(page.Turns) != 1 || page.NextBefore != -1 || !reflect.DeepEqual(page.Turns[0].Events, doc.CIR.Events[:2]) {
		t.Fatal("fresh page did not reset classification allowance", err)
	}
}

func TestHistoryProjectionOversizedTailAndMetadataProofReadNoChunks(t *testing.T) {
	ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, strings.Repeat("x", 4<<20)), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
	spy := &historyReadSpy{FSStore: st}
	svc, req := NewService(st, spy, nil, nil, nil), historyProjectionRequest()
	page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if !errors.Is(err, domain.ErrContextBudgetExceeded) || page.OmittedTail != nil || spy.whole != 0 || spy.chunks != 0 || !strings.Contains(err.Error(), "classification") {
		t.Fatalf("unclassified huge tail was read or omitted: tail=%+v chunks=%d err=%v", page.OmittedTail, spy.chunks, err)
	}
	req.Before, req.MaxBytes = 0, 1
	page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || page.Version != domain.AgentHistoryProjectionVersion || page.Before != 0 || page.OmittedTail != nil || page.Covered || len(page.Turns) != 0 || page.NextBefore != -1 || spy.chunks != 0 {
		t.Fatalf("metadata inspected a tail: %+v %v", page, err)
	}
	newer := putHistoryDoc(t, st, repo, historyEnvelope(), append(append([]domain.CIREvent(nil), doc.CIR.Events...), domain.CIREvent{Kind: domain.EventToolResult, CallID: "open", Output: "completed later"})...)
	req.Before, req.CoveredBy = -1, newer.Hash
	page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || page.Version != domain.AgentHistoryProjectionVersion || !page.Covered || page.OmittedTail != nil || len(page.Turns) != 0 || page.NextBefore != -1 || spy.chunks != 0 || spy.whole != 0 {
		t.Fatalf("prefix proof read or reclassified an incomplete old capture: %+v %v", page, err)
	}
}

func TestHistoryProjectionRejectsCorruptTailAndInvalidOption(t *testing.T) {
	ctx, repo, st := systemTestContext(), h('1'), store.NewFSStore(t.TempDir())
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "tail"), domain.CIREvent{Kind: domain.EventToolCall, CallID: "open", ToolName: "read"})
	for _, chunk := range []bool{false, true} {
		spy := &historyReadSpy{FSStore: st, tamperChunk: chunk, tamperEvent: !chunk}
		if _, err := NewService(st, spy, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, historyProjectionRequest()); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("corrupt tail was eligible for omission: %v", err)
		}
	}
	for _, mode := range []string{"strict", "OMIT", "true", "omit "} {
		req := historyProjectionRequest()
		req.IncompleteTail = mode
		spy := &historyReadSpy{FSStore: st}
		if _, err := NewService(st, spy, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrValidation) || spy.chunks != 0 {
			t.Fatalf("invalid mode %q read history: %v", mode, err)
		}
	}
}
