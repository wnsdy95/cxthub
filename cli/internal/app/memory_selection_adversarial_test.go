package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestReviewDedicatedMemorySelectionRejectsNonRootSibling(t *testing.T) {
	for _, consumer := range []string{"PR", "tracking"} {
		for _, mode := range []string{"root-only", "binding-only", "later-ordinary", "later-invalid-dedicated"} {
			t.Run(consumer+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				f := newPRPositionMemoryFixture(t)
				f.receipt.SourceBranchID = domain.LegacyContextBranchID(f.receipt.RepoID, "feature")
				empty := f.observation("")
				f.history.events = append(f.history.events, empty)
				m1 := f.memory(f.receipt.Source, "", "first root")
				root := f.observation(m1)
				root.MemorySelectionParent = empty.ID
				if mode == "binding-only" {
					root.BindingParent, root.MemorySelectionParent = empty.ID, ""
				}
				f.history.events = append(f.history.events, root)
				expected := m1
				if mode == "later-ordinary" || mode == "later-invalid-dedicated" {
					expected = f.memory(f.receipt.Source, m1, "later descendant")
					later := f.observation(expected)
					if mode == "later-invalid-dedicated" {
						later.MemorySelectionParent = empty.ID
					}
					f.history.events = append(f.history.events, later)
				}
				var got domain.ContentHash
				var err error
				if consumer == "PR" {
					p, e := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, f.history.events)
					got, err = p.MemoryHash, e
				} else {
					history := append([]domain.HistoryEvent(nil), f.history.events...)
					f.history.events = nil // real fresh-local observation path
					snap, e := f.store.GetSnapshot(ctx, f.receipt.Source)
					if e != nil {
						t.Fatal(e)
					}
					ref := domain.Ref{RepoID: f.receipt.RepoID, Kind: domain.RefBranch, Name: "feature", BranchID: f.receipt.SourceBranchID, Target: f.receipt.Source}
					event := empty
					event.ID, event.Kind = strings.Repeat("9", 32), "attach"
					a, e := f.svc.PrepareTrackingAttachment(ctx, event, inbound.RemoteBranchObservation{Ref: ref, History: history, Snapshots: []domain.Snapshot{snap}}, []string{empty.GitAfter})
					got, err = a.Event.MemoryHash, e
				}
				wantError := mode == "binding-only" || mode == "later-invalid-dedicated"
				if wantError && err == nil {
					t.Fatalf("accepted invalid %s relation; selected later digest=%v", mode, got == expected)
				}
				if !wantError && (err != nil || got != expected) {
					t.Fatalf("valid selection failed: memory=%s want=%s err=%v", got, expected, err)
				}
			})
		}
	}
}
