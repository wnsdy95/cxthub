package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func tailHistoryFixture(t *testing.T, paged bool, docs ...domain.SessionDoc) (*AgentContextService, inbound.PrepareAgentContextInput, *agentPageFixture) {
	t.Helper()
	s, in, h, memory, store := agentServiceFixture(t)
	memory.items = nil
	h.view.Snapshots = nil
	store.docs = map[domain.ContentHash]domain.SessionDoc{}
	for _, doc := range docs {
		store.docs[doc.Hash] = doc
		h.view.Snapshots = append(h.view.Snapshots, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: in.RepoID})
	}
	h.view.Position, in.SnapshotID = docs[0].Hash, docs[0].Hash
	in.ArtifactOnly = true
	in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 100000, Source: "explicit"}
	pages := &agentPageFixture{docs: store.docs}
	if paged {
		s.documents = pages
	}
	return s, in, pages
}

func unfinishedHistory(t *testing.T) domain.SessionDoc {
	t.Helper()
	events := []domain.Event{
		agentMessage("user", "complete earlier request", 0), agentMessage("assistant", "earlier response", 1),
		agentMessage("user", "unfinished-private-turn", 2),
		{Kind: domain.EventToolCall, CallID: "read", ToolName: "read", Seq: 3},
		{Kind: domain.EventToolResult, CallID: "read", Output: "read complete", Seq: 4},
		agentMessage("assistant", "continue the same request", 5),
		{Kind: domain.EventToolCall, CallID: "commit", ToolName: "shell", Seq: 6},
	}
	for i := range events {
		events[i].ID = fmt.Sprintf("event-%d", i)
	}
	return agentDocument(t, "same-native-session", events...)
}

func TestAgentHistoryTailProjectionPreservesRawTurnsAndStrictReader(t *testing.T) {
	doc := unfinishedHistory(t)
	raw, _ := json.Marshal(doc)
	if _, err := agentHistoryTurns(doc.CIR); !errors.Is(err, domain.ErrInvalidCIR) {
		t.Fatalf("strict projection accepted unfinished input: %v", err)
	}
	turns, tail, err := projectAgentHistoryTurns(doc.CIR, true)
	if err != nil || len(turns) != 1 || tail == nil || tail.Start != 2 || tail.End != 7 || tail.Reason != "incomplete_tool_pair" {
		t.Fatalf("bad tail projection: turns=%d tail=%+v err=%v", len(turns), tail, err)
	}
	if !reflect.DeepEqual(turns[0].Events, doc.CIR.Events[:2]) {
		t.Fatal("changed complete events")
	}
	after, _ := json.Marshal(doc)
	if string(raw) != string(after) {
		t.Fatal("mutated source archive")
	}
	for _, extra := range []domain.Event{
		agentMessage("user", "later request", 7),
		{Kind: domain.EventTurn, Role: "user", Seq: 7},
		{Kind: domain.EventToolCall, CallID: "commit", ToolName: "shell", Seq: 7},
		{Kind: domain.EventToolCall, ToolName: "shell", Seq: 7},
		{Kind: domain.EventToolResult, CallID: "unknown", Seq: 7},
		{Kind: domain.EventReasoning, Seq: -1},
		{Kind: domain.EventReasoning, Seq: 6},
		{Kind: "unknown", Seq: 7},
	} {
		bad := doc.CIR
		bad.Events = append(append([]domain.Event{}, bad.Events...), extra)
		if _, tail, err := projectAgentHistoryTurns(bad, true); err == nil || tail != nil {
			t.Fatal("interior/malformed pairing became an omitted tail")
		}
	}
	marker := doc.CIR
	marker.Events = append(append([]domain.Event{}, doc.CIR.Events[:2]...), append([]domain.Event{{Kind: domain.EventTurn, Role: "user", Seq: 2}}, doc.CIR.Events[2:]...)...)
	for i := range marker.Events {
		marker.Events[i].Seq = i
	}
	_, tail, err = projectAgentHistoryTurns(marker, true)
	if err != nil || tail == nil || tail.Start != 2 || tail.End != len(marker.Events) {
		t.Fatal("turn marker left outside omitted range")
	}
}

func TestAgentHistoryTailDoesNotResurrectBalancedPrefixAcrossDivergence(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			latest := unfinishedHistory(t)
			prefix := agentDocument(t, "same-native-session", latest.CIR.Events[:6]...)
			divergentEvents := append([]domain.Event{}, latest.CIR.Events[:2]...)
			divergentEvents = append(divergentEvents, agentMessage("user", "independent divergence", 2), agentMessage("assistant", "independent result", 3))
			divergent := agentDocument(t, "same-native-session", divergentEvents...)
			s, in, pages := tailHistoryFixture(t, paged, latest, divergent, prefix)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			prompt, _ := p.Prompt()
			if strings.Contains(prompt, "unfinished-private-turn") || !strings.Contains(prompt, "independent divergence") || len(p.Content.History) != 2 {
				t.Fatalf("excluded partial turn reappeared or valid divergence lost; segments=%d", len(p.Content.History))
			}
			found := false
			for _, gap := range p.Content.Gaps {
				if gap.Reason == "history_incomplete_tool_pair" {
					found = gap.Source != nil && gap.Source.SnapshotID == latest.Hash && gap.Source.DocHash == latest.Hash && gap.Source.StartEvent == 2 && gap.Source.EndEvent == 7
				}
			}
			if !found {
				t.Fatal("lost omission provenance")
			}
			if paged {
				proved := false
				for i, hash := range pages.requestedHashes {
					if hash == prefix.Hash {
						r := pages.requests[i]
						if r.Before != 0 {
							t.Fatal("read the excluded prefix body")
						}
						proved = proved || r.CoveredBy == latest.Hash
					}
				}
				if !proved {
					t.Fatal("divergence erased the omission anchor")
				}
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentHistoryTailCompletedLaterAndOtherSessionRemainIndependent(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			open := unfinishedHistory(t)
			done := agentDocument(t, "same-native-session", append(append([]domain.Event{}, open.CIR.Events...), domain.Event{Kind: domain.EventToolResult, CallID: "commit", Output: "ok", Seq: 7})...)
			other := agentDocument(t, "another-session", open.CIR.Events[:6]...)
			s, in, _ := tailHistoryFixture(t, paged, done, open, other)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Content.History) != 4 {
				t.Fatalf("lost completed turn or unrelated session: %d", len(p.Content.History))
			}
			for _, gap := range p.Content.Gaps {
				if gap.Reason == "history_incomplete_tool_pair" {
					t.Fatal("completed latest tip still omitted")
				}
			}
		})
	}
}

func TestAgentHistoryOnlyUnfinishedTailKeepsMemoryAndSource(t *testing.T) {
	for _, paged := range []bool{false, true} {
		open := agentDocument(t, "unfinished", agentMessage("user", "unfinished", 0), domain.Event{Kind: domain.EventToolCall, CallID: "running", ToolName: "shell", Seq: 1})
		s, in, _ := tailHistoryFixture(t, paged, open)
		p, err := s.PrepareAgentContext(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Content.History) != 0 || len(p.Content.Sources) == 0 {
			t.Fatal("invented complete history or lost source")
		}
		prompt, _ := p.Prompt()
		if !strings.Contains(prompt, "history_incomplete_tool_pair") || p.Usage.Tokens > p.EffectiveBudget() {
			t.Fatal("missing measured omission")
		}
	}
}

func TestAgentTailExclusionBoundsNeverDropEvidence(t *testing.T) {
	h := agentHash("event")
	e := agentTailExclusions{}
	for i := 0; i < maxAgentTailAnchors; i++ {
		if err := e.remember("s", agentTailAnchor{hash: h, total: 1, events: []domain.ContentHash{h}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.remember("s", agentTailAnchor{}); !errors.Is(err, domain.ErrAgentContextUnavailable) {
		t.Fatal("silently discarded an exclusion")
	}
	if ok, err := e.contains(context.Background(), "s", []domain.ContentHash{h}); err != nil || !ok {
		t.Fatal("lost prior proof")
	}
	e.proofs = maxAgentTailProofs
	if _, err := e.contains(context.Background(), "s", []domain.ContentHash{h}); !errors.Is(err, domain.ErrAgentContextUnavailable) {
		t.Fatal("unbounded prefix probes")
	}
	if err := (&agentTailExclusions{}).proof(maxAgentHistoryPrefixEvents + 1); !errors.Is(err, domain.ErrAgentContextUnavailable) {
		t.Fatal("treated unprovable prefix as independent")
	}
}

func TestAgentHistoryTailGapReselectsPreviouslyAcceptedTurns(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%t", paged), func(t *testing.T) {
			latest := agentDocument(t, "completed",
				agentMessage("user", "older complete "+strings.Repeat("a", 800), 0), agentMessage("assistant", "done", 1),
				agentMessage("user", "newest complete "+strings.Repeat("b", 800), 2), agentMessage("assistant", "done", 3))
			open := agentDocument(t, "unfinished", agentMessage("user", "not ready", 0), domain.Event{Kind: domain.EventToolCall, CallID: "open", Seq: 1})
			s, in, _ := tailHistoryFixture(t, paged, latest, open)
			s.tokens = agentTokenFixture{}
			baseline, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil || len(baseline.Content.History) != 2 {
				t.Fatalf("baseline: %v", err)
			}
			withoutGap := baseline
			withoutGap.Content.Gaps = nil
			for _, gap := range baseline.Content.Gaps {
				if gap.Reason != "history_incomplete_tool_pair" {
					withoutGap.Content.Gaps = append(withoutGap.Content.Gaps, gap)
				}
			}
			before, _ := withoutGap.Prompt()
			in.Policy.BudgetTokens = len(before)
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Content.History) != 1 || !reflect.DeepEqual(p.Content.History[0], baseline.Content.History[1]) || !reflect.DeepEqual(p.Content.Gaps, baseline.Content.Gaps) {
				t.Fatal("gap must retain provenance and drop only the oldest complete turn")
			}
			prompt, _ := p.Prompt()
			if p.Usage.Tokens != len(prompt) || p.Usage.Tokens > p.EffectiveBudget() {
				t.Fatal("unmeasured final diagnostic")
			}
		})
	}
}

func TestAgentTailExclusionHonorsCancellation(t *testing.T) {
	e := agentTailExclusions{}
	h := agentHash("event")
	if err := e.remember("s", agentTailAnchor{total: 1, events: []domain.ContentHash{h}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.contains(ctx, "s", []domain.ContentHash{h}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled proof became an independent source: %v", err)
	}
}
