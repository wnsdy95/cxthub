package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestInitialMemorySelectionRequiresExactCausalProof(t *testing.T) {
	for _, mode := range []string{"exact", "unlinked", "binding parent only", "different worktree", "different alias", "birth parent", "attach parent", "advance child", "not first digest", "foreign owner", "contradictory owner", "unrelated empty", "divergent successors", "missing parent"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			empty := f.observation("")
			f.history.events = append(f.history.events, empty)
			memory := f.memory(f.receipt.Source, "", "first memory")
			next := f.observation(memory)
			next.MemorySelectionParent = empty.ID
			// Wall clocks intentionally disagree with the explicit causal edge.
			next.CreatedAt = empty.CreatedAt.Add(-time.Hour)
			switch mode {
			case "binding parent only":
				next.BindingParent, next.MemorySelectionParent = empty.ID, ""
			case "unlinked":
				next.MemorySelectionParent = ""
			case "different worktree":
				next.WorktreeID = strings.Repeat("6", 32)
			case "different alias":
				next.LocalBranch = "other-alias"
			case "birth parent":
				f.history.events[0].Kind = "birth"
			case "attach parent":
				f.history.events[0].Kind = "attach"
			case "advance child":
				next.Kind = "advance"
			case "not first digest":
				next.MemoryHash = f.memory(f.receipt.Source, memory, "later memory")
			case "foreign owner":
				owner := f.snapshot(f.receipt.RepoID, "foreign memory owner")
				next.MemoryHash = f.memory(owner, "", "foreign root")
			case "contradictory owner":
				next.MemorySource = f.snapshot(f.receipt.RepoID, "contradictory explicit owner")
			case "unrelated empty":
				other := empty
				other.ID = strings.Repeat("9", 32)
				f.history.events = append(f.history.events, other)
			case "divergent successors":
				other := next
				other.ID = strings.Repeat("9", 32)
				other.MemoryHash = f.memory(f.receipt.Source, "", "independent root")
				f.history.events = append(f.history.events, other)
			case "missing parent":
				next.MemorySelectionParent = strings.Repeat("9", 32)
			}
			f.history.events = append(f.history.events, next)
			original := append([]domain.HistoryEvent(nil), f.history.events...)
			for _, reverse := range []bool{false, true} {
				if reverse {
					for i, j := 0, len(f.history.events)-1; i < j; i, j = i+1, j-1 {
						f.history.events[i], f.history.events[j] = f.history.events[j], f.history.events[i]
					}
				}
				p, err := f.svc.ResolvePRSourcePosition(context.Background(), f.receipt)
				if mode == "exact" {
					if err != nil || p.MemoryHash != memory || p.MemorySource != "" || !p.MemoryPinned {
						t.Fatalf("exact first-memory transition: %+v %v", p, err)
					}
				} else if err == nil {
					t.Fatalf("accepted %s: %+v", mode, p)
				}
			}
			for i, j := 0, len(f.history.events)-1; i < j; i, j = i+1, j-1 {
				f.history.events[i], f.history.events[j] = f.history.events[j], f.history.events[i]
			}
			if !reflect.DeepEqual(original, f.history.events) {
				t.Fatal("immutable evidence was changed")
			}
		})
	}
}
