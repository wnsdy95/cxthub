package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func preparePagedHistoryFixture(t *testing.T, docs ...domain.SessionDoc) (*AgentContextService, inbound.PrepareAgentContextInput, *agentPageFixture, *agentHistoryFixture) {
	t.Helper()
	s, in, history, _, _ := agentServiceFixture(t)
	pages := &agentPageFixture{docs: map[domain.ContentHash]domain.SessionDoc{}}
	history.view.Snapshots = nil
	for _, doc := range docs {
		pages.docs[doc.Hash] = doc
		// Deliberately omit native identity labels. The document metadata, not
		// snapshot labels, selects the backend's independently verified proof.
		history.view.Snapshots = append(history.view.Snapshots, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID})
	}
	history.view.Position, in.SnapshotID = docs[0].Hash, docs[0].Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 200000, Source: "explicit"}
	s.documents = pages
	return s, in, pages, history
}

func TestAgentContextPagedProvesCoverageBeforeReadingIncompleteCapture(t *testing.T) {
	first := []domain.Event{
		agentMessage("user", "inspect", 0),
		{Kind: domain.EventToolCall, Seq: 1, CallID: "c", ToolName: "read"},
		{Kind: domain.EventToolResult, Seq: 2, CallID: "c", Output: "done"},
	}
	latest := agentDocument(t, "native", first...)
	older := agentDocument(t, "native", first[:2]...)
	s, in, pages, _ := preparePagedHistoryFixture(t, latest, older)
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal("covered incomplete capture must never be read as a turn", err)
	}
	if len(p.Content.History) != 1 || len(p.Content.History[0].Events) != 3 || pages.readEvents != 3 {
		t.Fatalf("duplicate or incomplete source read: history=%+v reads=%d", p.Content.History, pages.readEvents)
	}
	if pages.calls != 3 || pages.requestedHashes[1] != older.Hash || pages.requests[1].Before != 0 || pages.requests[1].CoveredBy != "" || pages.requests[2].Before != -1 || pages.requests[2].CoveredBy != latest.Hash {
		t.Fatalf("expected identity-only probe then proof-bearing first body request: %+v", pages.requests)
	}
}

func TestAgentContextPagedCannotUseSnapshotLabelsForCoverage(t *testing.T) {
	first := []domain.Event{agentMessage("user", "inspect", 0), {Kind: domain.EventToolCall, Seq: 1, CallID: "c", ToolName: "read"}, {Kind: domain.EventToolResult, Seq: 2, CallID: "c", Output: "done"}}
	latest := agentDocument(t, "alice", first...)
	foreign := agentDocument(t, "bob", first[:2]...)
	s, in, pages, history := preparePagedHistoryFixture(t, latest, foreign)
	for i := range history.view.Snapshots {
		history.view.Snapshots[i].Provider = domain.ProviderCodex
		history.view.Snapshots[i].SessionID = "alice"
	}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 1 || p.Content.History[0].SessionID != "alice" {
		t.Fatal("uncovered incomplete foreign session became selected history")
	}
	found := false
	for _, gap := range p.Content.Gaps {
		if gap.Reason == "history_incomplete_tool_pair" && gap.Source != nil && gap.Source.DocHash == foreign.Hash {
			found = true
		}
	}
	if !found {
		t.Fatal("foreign session was falsely covered instead of separately omitted")
	}
	if len(pages.requests) != 3 || pages.requests[2].CoveredBy != "" {
		t.Fatalf("snapshot hint selected native-session proof: %+v", pages.requests)
	}
}

func TestAgentContextPagedPinsMetadataThroughCoverageRead(t *testing.T) {
	first := []domain.Event{agentMessage("user", "first", 0), agentMessage("user", "second", 1)}
	latest, older := agentDocument(t, "native", first...), agentDocument(t, "native", first[:1]...)
	for _, mode := range []string{"session", "provider", "total", "revoked metadata", "revoked proof"} {
		t.Run(mode, func(t *testing.T) {
			s, in, pages, _ := preparePagedHistoryFixture(t, latest, older)
			pages.mutate = func(page *domain.AgentHistoryPage) {
				if page.Hash != older.Hash || page.Before != 0 {
					return
				}
				switch mode {
				case "session":
					page.SessionID = "changed"
				case "provider":
					page.Provider = domain.ProviderClaude
				case "total":
					page.Total++
				}
			}
			if mode == "revoked metadata" {
				pages.failAt = 2
			}
			if mode == "revoked proof" {
				pages.failAt = 3
			}
			if _, err := s.PrepareAgentContext(context.Background(), in); err == nil {
				t.Fatal("accepted changed/revoked source metadata")
			}
		})
	}
}

func TestAgentContextPagedIdentityConflictsDoNotDependOnPageBoundaries(t *testing.T) {
	for _, mode := range []string{"same turn", "same page", "different pages"} {
		t.Run(mode, func(t *testing.T) {
			events := []domain.Event{agentMessage("user", "first body", 0), agentMessage("user", "different body", 1)}
			if mode == "same turn" {
				events[1].Role = "assistant"
			}
			if mode == "different pages" {
				for i := 2; i < 17; i++ {
					events = append(events, agentMessage("user", fmt.Sprintf("body-%d", i), i))
				}
			}
			events[0].ID, events[len(events)-1].ID = "reused", "reused"
			doc := agentDocument(t, "native", events...)
			s, in, _, _ := preparePagedHistoryFixture(t, doc)
			if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("conflicting native identity accepted: %v", err)
			}
		})
	}
}

func TestAgentContextPagedDedupAndSegmentOrderAcrossDocumentsAndPages(t *testing.T) {
	var events []domain.Event
	for i := 0; i < 40; i++ {
		ev := agentMessage("user", fmt.Sprintf("turn-%d", i), i)
		ev.ID = fmt.Sprintf("native-%d", i)
		events = append(events, ev)
	}
	// These same-session sources overlap without being complete prefixes. The
	// newer document supplies 10..39; the older contributes only missing 0..9.
	latest := agentDocument(t, "native", events[10:]...)
	older := agentDocument(t, "native", events[:20]...)
	s, in, pages, _ := preparePagedHistoryFixture(t, latest, older)
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 40 {
		t.Fatalf("overlap duplicated or omitted: %d", len(p.Content.History))
	}
	for i, segment := range p.Content.History {
		if len(segment.Events) != 1 || segment.Events[0].Seq != i || segment.Events[0].ID != events[i].ID {
			t.Fatalf("segment %d out of chronological order: %+v", i, segment)
		}
		wantHash, ordinal := latest.Hash, i-10
		if i < 10 {
			wantHash, ordinal = older.Hash, i
		}
		if segment.Source.DocHash != wantHash || segment.Source.StartEvent != ordinal || segment.Source.EndEvent != ordinal+1 {
			t.Fatalf("segment %d points to wrong source range: %+v", i, segment.Source)
		}
	}
	if pages.calls != 5 {
		t.Fatalf("expected four body pages and one metadata read, got %d", pages.calls)
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentContextPagedRetainsSameEventIDInDifferentNativeSessions(t *testing.T) {
	newer := agentMessage("user", "new body", 0)
	older := agentMessage("user", "different old body", 0)
	newer.ID, older.ID = "same-id", "same-id"
	s, in, _, _ := preparePagedHistoryFixture(t, agentDocument(t, "alice", newer), agentDocument(t, "bob", older))
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Content.History) != 2 || p.Content.History[0].SessionID != "bob" || p.Content.History[1].SessionID != "alice" {
		t.Fatal("cross-session identity was merged or reordered", p.Content.History)
	}
}

func TestAgentContextPagedNativeIdentityKeysDoNotAliasSeparators(t *testing.T) {
	newer, older := agentMessage("user", "new body", 0), agentMessage("user", "old body", 0)
	newer.ID, older.ID = "b\x00c", "c"
	s, in, _, _ := preparePagedHistoryFixture(t, agentDocument(t, "a", newer), agentDocument(t, "a\x00b", older))
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal("distinct native identities aliased", err)
	}
	if len(p.Content.History) != 2 {
		t.Fatal("different native sessions were deduplicated")
	}
}

func TestAgentContextHistoryFilterClearsDecodedV2PresenceWithoutChangingArchive(t *testing.T) {
	for _, mode := range []string{"paged with IDs", "paged without IDs", "full document"} {
		t.Run(mode, func(t *testing.T) {
			const wire = `{"envelope":{"cir_version":"2","source_provider":"codex","session_origin_id":"v2-native"},"events":[
			 {"kind":"message","id":"u","seq":0,"role":"user","blocks":[{"type":"text","text":"inspect <>&"}],"provider_metadata":{"turn_id":"t","create_time":9007199254740993}},
			 {"kind":"message","id":"a","seq":1,"role":"assistant","blocks":[{"type":"text","text":"visible agent answer"}],"agent_message":true,"agent_author":"/reviewer","agent_recipient":"/main","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}},
			 {"kind":"tool_call","id":"c","seq":2,"call_id":"call","tool_name":"read","provider_tool_name":"native_read","status":"completed","input":{"path":"sample"},"provider_metadata":{"turn_id":"t"}},
			 {"kind":"tool_result","id":"r","seq":3,"call_id":"call","output":{"text":"result"},"is_error":true,"provider_metadata":{"turn_id":"t"}}
			]}`
			var cir domain.CIRDocument
			if err := json.Unmarshal([]byte(wire), &cir); err != nil {
				t.Fatal(err)
			}
			if mode == "paged without IDs" {
				for i := range cir.Events {
					cir.Events[i].ID = ""
				}
			}
			raw, err := domain.CanonicalBytes(cir)
			if err != nil {
				t.Fatal(err)
			}
			doc := domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir}
			s, in, _, _ := preparePagedHistoryFixture(t, doc)
			if mode == "full document" {
				s.documents = &agentDocFixture{docs: map[domain.ContentHash]domain.SessionDoc{doc.Hash: doc}}
			}
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal("valid decoded v2 evidence became unmarshalable", err)
			}
			if len(p.Content.History) != 1 || len(p.Content.History[0].Events) != 4 {
				t.Fatal("lost visible evidence")
			}
			selected := p.Content.History[0].Events
			for i, ev := range selected {
				if ev.ID != cir.Events[i].ID || ev.Seq != cir.Events[i].Seq {
					t.Fatal("changed event identity or sequence")
				}
			}
			if selected[0].Blocks[0].Text != "inspect <>&" || selected[1].Blocks[0].Text != "visible agent answer" || selected[2].CallID != "call" || selected[2].ProviderToolName != "native_read" || selected[2].Status != "completed" || selected[2].Input["path"] != "sample" || selected[3].CallID != "call" || !selected[3].IsError {
				t.Fatal("changed historical message/tool evidence")
			}
			projected, err := json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"provider_metadata", "agent_message", "agent_author", "agent_recipient", "locked", "opaque"} {
				if strings.Contains(string(projected), field) {
					t.Fatalf("retained native field %s", field)
				}
			}
			after, err := domain.CanonicalBytes(doc.CIR)
			if err != nil || string(after) != string(raw) {
				t.Fatal("archive mutated during projection", err)
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
