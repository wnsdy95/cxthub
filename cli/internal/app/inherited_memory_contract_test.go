package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// RED on ad9269c: existing APIs compile, but the explicit inherited->own-root
// selection dependency is rejected at runtime. No mutable attachment is set.
func TestInheritedMemorySelectionAcceptance(t *testing.T) {
	for _, consumer := range []string{"PR", "tracking"} {
		for _, source := range []string{"target", "prior-context"} {
			t.Run(consumer+"/"+source, func(t *testing.T) {
				ctx := context.Background()
				f := newPRPositionMemoryFixture(t)
				f.receipt.SourceBranchID = domain.LegacyContextBranchID(f.receipt.RepoID, "feature")
				owner := f.snapshot(f.receipt.RepoID, "inherited-owner")
				ma := f.memory(owner, "", "inherited-root")
				before := f.observation(ma)
				before.MemorySource = owner
				if source == "prior-context" {
					before.Source = owner
				}
				f.history.events = append(f.history.events, before)
				mb := f.memory(f.receipt.Source, "", "first-self-owned-root")
				after := f.observation(mb)
				after.MemorySelectionParent = before.ID
				after.CreatedAt = before.CreatedAt.Add(-time.Hour)
				f.history.events = append(f.history.events, after)
				for _, e := range f.history.events {
					if _, err := f.svc.ValidateHistorySource(ctx, e); err != nil {
						t.Fatalf("ordinary source setup invalid: %v", err)
					}
				}
				frozen := append([]domain.HistoryEvent(nil), f.history.events...)
				var got domain.ContentHash
				var err error
				if consumer == "PR" {
					p, e := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, f.history.events)
					got, err = p.MemoryHash, e
				} else {
					observed := append([]domain.HistoryEvent(nil), f.history.events...)
					f.history.events = nil
					var snapshots []domain.Snapshot
					for _, id := range []domain.ContentHash{f.receipt.Source, owner} {
						snap, e := f.store.GetSnapshot(ctx, id)
						if e != nil {
							t.Fatal(e)
						}
						snapshots = append(snapshots, snap)
					}
					ref := domain.Ref{RepoID: f.receipt.RepoID, Kind: domain.RefBranch, Name: "feature", BranchID: f.receipt.SourceBranchID, Target: f.receipt.Source}
					input := before
					input.ID = strings.Repeat("9", 32)
					input.Kind = "attach"
					a, e := f.svc.PrepareTrackingAttachment(ctx, input, inbound.RemoteBranchObservation{Ref: ref, History: observed, Snapshots: snapshots}, []string{before.GitAfter})
					got, err = a.Event.MemoryHash, e
					if !reflect.DeepEqual(observed, frozen) {
						t.Fatal("raw observations changed")
					}
				}
				if err != nil || got != mb {
					t.Fatalf("exact inherited ordinary predecessor rejected: selected=%s want=%s error=%v", got, mb, err)
				}
				if consumer == "PR" && !reflect.DeepEqual(f.history.events, frozen) {
					t.Fatal("raw observations changed")
				}
			})
		}
	}
}
