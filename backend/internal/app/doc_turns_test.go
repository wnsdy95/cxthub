package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyReadSpy struct {
	*store.FSStore
	whole, chunks            int
	forbidden                domain.ContentHash
	tamperChunk, tamperEvent bool
}

func (s *historyReadSpy) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.whole++
	return domain.SessionDoc{}, errors.New("whole document read forbidden")
}
func (s *historyReadSpy) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	s.chunks++
	if hash == s.forbidden {
		return nil, errors.New("unused early source chunk read")
	}
	b, err := s.FSStore.GetChunk(ctx, repo, hash)
	if s.tamperChunk && len(b) > 0 {
		b[0] ^= 1
	}
	return b, err
}
func (s *historyReadSpy) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	idx, err := s.FSStore.DocReadIndex(ctx, repo, hash)
	if s.tamperEvent && len(idx.Events) > 0 {
		idx.Events[len(idx.Events)-1].Hash = domain.HashContent([]byte("tampered event"))
	}
	return idx, err
}

func historyMessage(role domain.Role, text string) domain.CIREvent {
	return domain.CIREvent{Kind: domain.EventMessage, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
}
func putHistoryDoc(t *testing.T, st *store.FSStore, repo domain.ContentHash, env domain.CIREnvelope, events ...domain.CIREvent) domain.SessionDoc {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: env, Events: append([]domain.CIREvent{}, events...)}}
	for i := range doc.CIR.Events {
		doc.CIR.Events[i].Seq = i
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(systemTestContext(), repo, doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func historyEnvelope() domain.CIREnvelope {
	return domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "native-history"}
}

func TestHistoryPageMetadataBeforeIncompleteCaptureCoverageReadsNoBodies(t *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	st := store.NewFSStore(t.TempDir())
	latest := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "inspect"),
		domain.CIREvent{Kind: domain.EventToolCall, CallID: "c", ToolName: "read"},
		domain.CIREvent{Kind: domain.EventToolResult, CallID: "c", Output: "done"})
	older := putHistoryDoc(t, st, repo, historyEnvelope(), latest.CIR.Events[:2]...)
	spy := &historyReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	metadata, err := svc.ReadAgentHistoryPage(ctx, repo, older.Hash, domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1})
	if err != nil || metadata.Provider != domain.ProviderCodex || metadata.SessionID != historyEnvelope().SessionOriginID || metadata.Total != 2 || metadata.Before != 0 || metadata.NextBefore != -1 || len(metadata.Turns) != 0 || metadata.Covered {
		t.Fatalf("metadata read of incomplete capture: %+v %v", metadata, err)
	}
	proofReq := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 1, CoveredBy: latest.Hash}
	page, err := svc.ReadAgentHistoryPage(ctx, repo, older.Hash, proofReq)
	if err != nil || !page.Covered || len(page.Turns) != 0 || spy.whole != 0 || spy.chunks != 0 {
		t.Fatalf("proof must precede body/pair/budget checks: %+v %v whole=%d chunks=%d", page, err, spy.whole, spy.chunks)
	}
	// Matching event hashes and client-selected candidates never substitute for
	// the backend's native-session check, even for an incomplete older capture.
	otherEnv := historyEnvelope()
	otherEnv.SessionOriginID = "different-native-session"
	foreignSession := putHistoryDoc(t, st, repo, otherEnv, latest.CIR.Events...)
	proofReq.CoveredBy, proofReq.MaxBytes = foreignSession.Hash, 4<<20
	page, err = svc.ReadAgentHistoryPage(ctx, repo, older.Hash, proofReq)
	if !errors.Is(err, domain.ErrIntegrity) || page.Covered {
		t.Fatalf("cross-session hint bypassed incomplete-pair validation: %+v %v", page, err)
	}
	proofReq.CoveredBy = ""
	if _, err = svc.ReadAgentHistoryPage(ctx, repo, older.Hash, proofReq); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("uncovered incomplete capture was silently accepted: %v", err)
	}
}

func TestHistoryPageNewestBoundedTurnsDoNotReadHugeEarlySource(t *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	st := store.NewFSStore(t.TempDir())
	doc := putHistoryDoc(t, st, repo, historyEnvelope(),
		historyMessage(domain.RoleUser, strings.Repeat("huge unused source ", 350000)),
		historyMessage(domain.RoleAssistant, "old answer"),
		historyMessage(domain.RoleUser, "second prompt"),
		domain.CIREvent{Kind: domain.EventToolCall, CallID: "call", ToolName: "read", Input: map[string]any{"path": "sample"}},
		domain.CIREvent{Kind: domain.EventToolResult, CallID: "call", Output: "result"},
		historyMessage(domain.RoleAssistant, "second answer"),
		domain.CIREvent{Kind: domain.EventTurn, Role: domain.RoleUser},
		historyMessage(domain.RoleUser, "newest prompt <>& \uD55C\uAD6D\uC5B4"),
		historyMessage(domain.RoleAssistant, "newest answer"))
	man, err := st.GetDocManifest(ctx, repo, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	spy := &historyReadSpy{FSStore: st, forbidden: man.Chunks[0]}
	svc := NewService(st, spy, nil, nil, nil)
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 1, MaxBytes: 4 << 20}
	page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil {
		t.Fatal(err)
	}
	if page.Before != 9 || page.Total != 9 || page.NextBefore != 6 || len(page.Turns) != 1 || page.Turns[0].Start != 6 || page.Turns[0].End != 9 || !reflect.DeepEqual(page.Turns[0].Events, doc.CIR.Events[6:]) {
		t.Fatalf("newest boundary: %+v", page)
	}
	wire, _ := json.Marshal(page.Turns[0].Events)
	if page.Turns[0].Hash != domain.HashContent(wire) || spy.whole != 0 || spy.chunks > 2 {
		t.Fatalf("unbounded or unverified read: %+v", spy)
	}
	req.Before = page.NextBefore
	page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || page.NextBefore != 2 || len(page.Turns) != 1 || page.Turns[0].Start != 2 || len(page.Turns[0].Events) != 4 {
		t.Fatalf("tool pair boundary: %+v %v", page, err)
	}
	req.Before = page.NextBefore
	before := spy.chunks
	if _, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatalf("oversized newest: %v", err)
	}
	if spy.chunks != before || spy.whole != 0 {
		t.Fatal("oversized source body was read")
	}
	for _, cursor := range []int{-2, 3, 7, 10} {
		req.Before = cursor
		if _, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("cursor %d: %v", cursor, err)
		}
	}
}

func TestHistoryPageByteBudgetIsExactWireBytesAndNeverSkipsNewest(t *testing.T) {
	st, repo, ctx := store.NewFSStore(t.TempDir()), h('1'), systemTestContext()
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "old"), historyMessage(domain.RoleAssistant, "answer"), historyMessage(domain.RoleUser, strings.Repeat("<>&", 20)), historyMessage(domain.RoleAssistant, "new answer"))
	svc := NewService(st, st, nil, nil, nil)
	raw, _ := json.Marshal(doc.CIR.Events[2:])
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: len(raw)}
	page, err := svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || len(page.Turns) != 1 || page.NextBefore != 2 {
		t.Fatalf("exact byte boundary: %+v %v", page, err)
	}
	req.MaxBytes--
	if _, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatalf("wire expansion bound: %v", err)
	}
	req.MaxBytes = 4 << 20
	page, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req)
	if err != nil || len(page.Turns) != 2 || page.NextBefore != -1 || page.Turns[0].Start != 2 || page.Turns[1].Start != 0 {
		t.Fatalf("newest-first exhausted: %+v %v", page, err)
	}
	for _, n := range []int{0, -1, (4 << 20) + 1} {
		req.MaxBytes = n
		if _, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrValidation) {
			t.Fatal(n, err)
		}
	}
}

func TestHistoryPageCoverageRequiresWholeSameNativeSessionPrefix(t *testing.T) {
	st, repo, ctx := store.NewFSStore(t.TempDir()), h('1'), systemTestContext()
	env := historyEnvelope()
	old := putHistoryDoc(t, st, repo, env, historyMessage(domain.RoleUser, "prompt"), historyMessage(domain.RoleAssistant, "answer"))
	spy := &historyReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	for _, mode := range []string{"covered", "equal", "partial", "reordered", "cross session", "cross provider", "missing session"} {
		t.Run(mode, func(t *testing.T) {
			nextEnv := env
			events := append(append([]domain.CIREvent{}, old.CIR.Events...), historyMessage(domain.RoleUser, "later"))
			switch mode {
			case "covered":
				nextEnv.GitBranch = "another-branch"
			case "equal":
				events = events[:2]
			case "partial":
				events = events[:1]
			case "reordered":
				events[0], events[1] = events[1], events[0]
			case "cross session":
				nextEnv.SessionOriginID = "other"
			case "cross provider":
				nextEnv.SourceProvider = domain.ProviderClaude
			case "missing session":
				nextEnv.SessionOriginID = ""
			}
			next := putHistoryDoc(t, st, repo, nextEnv, events...)
			before := spy.chunks
			page, err := svc.ReadAgentHistoryPage(ctx, repo, old.Hash, domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20, CoveredBy: next.Hash})
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "covered" || mode == "equal"
			if page.Covered != want || want && (len(page.Turns) != 0 || page.NextBefore != -1 || spy.chunks != before) || !want && len(page.Turns) != 1 {
				t.Fatalf("coverage %+v", page)
			}
		})
	}
	foreign := putHistoryDoc(t, st, h('2'), env, historyMessage(domain.RoleUser, "foreign"))
	for _, pair := range [][2]domain.ContentHash{{old.Hash, foreign.Hash}, {foreign.Hash, ""}} {
		_, err := svc.ReadAgentHistoryPage(ctx, repo, pair[0], domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20, CoveredBy: pair[1]})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("foreign document: %v", err)
		}
	}
	if spy.whole != 0 {
		t.Fatal("coverage fetched source bodies")
	}
	idx := domain.DocReadIndex{Envelope: env, Events: make([]domain.DocEventIndex, domain.MaxContextSegmentPrefixEvents+1)}
	if historyIndexCovered(ctx, idx, idx) {
		t.Fatal("prefix proof exceeded bounded event count")
	}
	idx.Events = idx.Events[:domain.MaxContextSegmentPrefixEvents]
	if !historyIndexCovered(ctx, idx, idx) {
		t.Fatal("proof limit excluded exact boundary")
	}
}

func TestHistoryPageRejectsCorruptChunksEventsAndBrokenToolPairs(t *testing.T) {
	st, repo, ctx := store.NewFSStore(t.TempDir()), h('1'), systemTestContext()
	doc := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "prompt"), historyMessage(domain.RoleAssistant, "answer"))
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20}
	for _, mode := range []string{"chunk", "event"} {
		spy := &historyReadSpy{FSStore: st, tamperChunk: mode == "chunk", tamperEvent: mode == "event"}
		if _, err := NewService(st, spy, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	for _, events := range [][]domain.CIREvent{
		{historyMessage(domain.RoleUser, "prompt"), {Kind: domain.EventToolCall, CallID: "c", ToolName: "read"}},
		{historyMessage(domain.RoleUser, "prompt"), {Kind: domain.EventToolResult, CallID: "c", Output: "oops"}},
		{historyMessage(domain.RoleUser, "prompt"), {Kind: domain.EventToolCall, CallID: "c", ToolName: "read"}, historyMessage(domain.RoleUser, "split"), {Kind: domain.EventToolResult, CallID: "c", Output: "oops"}},
	} {
		doc = putHistoryDoc(t, st, repo, historyEnvelope(), events...)
		if _, err := NewService(st, st, nil, nil, nil).ReadAgentHistoryPage(ctx, repo, doc.Hash, req); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("broken tool pair: %v", err)
		}
	}
}
