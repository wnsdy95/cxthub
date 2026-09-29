package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentContextHistoricalMemoryPinUsesExactOwnerAndHash(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected owner", true: "ancestor owner"}[ancestor], func(t *testing.T) {
			s, in, h, _, _ := agentServiceFixture(t)
			owner := in.SnapshotID
			if ancestor {
				owner = agentHash("ancestor")
				h.view.Snapshots = append(h.view.Snapshots, domain.Snapshot{ID: owner, DocHash: owner, RepoID: in.RepoID})
			}
			pin := domain.AgentMemoryPin{SnapshotID: owner, MemoryHash: agentHash("historical memory")}
			in.MemoryPin = &pin
			in.Branch = "main"
			calls := 0
			s.memory = promptReadFunc(func(_ context.Context, _ string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				if req.Selection.SnapshotID != owner || req.Selection.MemoryHash != pin.MemoryHash || req.Selection.Branch != "" {
					t.Fatalf("historical memory replaced by current projection: %+v", req.Selection)
				}
				page := domain.EffectiveMemoryPage{Content: req.Content, Selection: req.Selection, StateHash: agentHash("pinned assessment"), LineageHash: pin.MemoryHash, Total: 1, Items: []domain.EffectiveMemoryItem{{ID: agentHash("old decision"), SourceSnapshot: owner, Kind: "decision", State: "retained", Reason: "project_decision", Text: "Decision at selected time"}}}
				page.Revision.Graph, page.Revision.Evidence = h.view.Revision.Graph, h.view.Revision.Evidence
				return page, nil
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !reflect.DeepEqual(p.Content.Selection.MemoryPin, &pin) || len(p.Content.ProjectMemory) != 1 {
				t.Fatalf("pin not retained and reauthorized: %+v, reads=%d", p, calls)
			}
			for _, source := range p.Content.Sources {
				if source.MemoryHash != "" && (source.SnapshotID != owner || source.MemoryHash != pin.MemoryHash) {
					t.Fatalf("source pointer advertises a later attachment: %+v", source)
				}
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			// Input mutation must not rewrite the receipt after it was measured.
			pin.MemoryHash = agentHash("later memory")
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal("receipt aliases caller's pin", err)
			}
		})
	}
}

func TestAgentContextExplicitEmptyMemoryPinNeverQueriesLatest(t *testing.T) {
	s, in, h, memory, _ := agentServiceFixture(t)
	in.MemoryPin = &domain.AgentMemoryPin{}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if memory.calls != 0 || h.calls != 2 || p.Content.Selection.MemoryPin == nil || len(p.Content.ProjectMemory) != 0 || p.Content.Selection.MemoryStateHash != "" {
		t.Fatalf("empty pin queried or imported mutable memory: %+v calls=%d", p, memory.calls)
	}
	for _, source := range p.Content.Sources {
		if source.MemoryHash != "" {
			t.Fatal("empty pin advertises current memory", source)
		}
	}
	prompt, _ := p.Prompt()
	if !strings.Contains(prompt, "historical_memory_empty") {
		t.Fatal("missing empty-pin explanation")
	}
	h.after = func(v *domain.HistoryQueryResult) { v.StateHash = agentHash("changed") }
	h.calls = 0
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("empty memory skipped context reauthorization", err)
	}
}

func TestAgentContextPinnedMemoryNeverFallsBackOnMissingOrWrongVersion(t *testing.T) {
	for _, failure := range []string{"missing", "wrong lineage", "wrong echo", "revoked"} {
		t.Run(failure, func(t *testing.T) {
			s, in, h, _, _ := agentServiceFixture(t)
			in.MemoryPin = &domain.AgentMemoryPin{SnapshotID: in.SnapshotID, MemoryHash: agentHash("old memory")}
			calls := 0
			s.memory = promptReadFunc(func(_ context.Context, _ string, req domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
				calls++
				if req.Selection.MemoryHash != in.MemoryPin.MemoryHash {
					t.Fatal("fell back to latest memory")
				}
				if failure == "missing" || failure == "revoked" && calls > 1 {
					return domain.EffectiveMemoryPage{}, domain.ErrNotFound
				}
				page := domain.EffectiveMemoryPage{Content: req.Content, Selection: req.Selection, StateHash: agentHash("state"), LineageHash: in.MemoryPin.MemoryHash}
				page.Revision.Graph, page.Revision.Evidence = h.view.Revision.Graph, h.view.Revision.Evidence
				if failure == "wrong lineage" {
					page.LineageHash = agentHash("new memory")
				}
				if failure == "wrong echo" {
					page.Selection.MemoryHash = ""
				}
				return page, nil
			})
			if _, err := s.PrepareAgentContext(context.Background(), in); err == nil {
				t.Fatal("accepted missing or substituted pinned memory")
			}
		})
	}
}

func TestAgentContextRejectsMalformedPinBeforeReading(t *testing.T) {
	for _, pin := range []domain.AgentMemoryPin{{SnapshotID: agentHash("owner")}, {MemoryHash: agentHash("memory")}, {SnapshotID: "invalid", MemoryHash: agentHash("memory")}} {
		s, in, h, m, _ := agentServiceFixture(t)
		in.MemoryPin = &pin
		if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrHashMismatch) || h.calls != 0 || m.calls != 0 {
			t.Fatalf("invalid pin read sources: %v %d %d", err, h.calls, m.calls)
		}
	}
}

func TestAgentContextRecheckRejectsSameCodeDifferentBranch(t *testing.T) {
	s, in, h, _, _ := agentServiceFixture(t)
	h.after = func(v *domain.HistoryQueryResult) { v.Selection.Branch = "other" }
	if _, err := s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("accepted another branch at the same SHA/revision", err)
	}
}
