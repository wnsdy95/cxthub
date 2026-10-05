package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Regression from the independent review, using actual stored memory objects.
func TestTrackingEquivalentSourceOwnedPin(t *testing.T) {
	for _, mode := range []string{"implicit-owner-control", "explicit-owner-control", "implicit-owner-with-normalized-local"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newPRPositionMemoryFixture(t)
			repo := f.receipt.RepoID
			owner := f.snapshot(repo, "legacy source owner")
			pin := f.memory(owner, "", "exact inherited memory")
			birth := f.observation(pin)
			birth.Kind, birth.Source = "birth", owner
			if mode == "explicit-owner-control" {
				birth.MemorySource = owner
			}
			if _, err := f.svc.ValidateHistorySource(ctx, birth); err != nil {
				t.Fatalf("supported source-owned remote evidence rejected: %v", err)
			}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: birth.Branch, BranchID: birth.BranchID, Target: birth.Target}
			targetSnapshot, err := f.store.GetSnapshot(ctx, birth.Target)
			if err != nil {
				t.Fatal(err)
			}
			ownerSnapshot, err := f.store.GetSnapshot(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			observed := inbound.RemoteBranchObservation{Ref: ref, History: []domain.HistoryEvent{birth}, Snapshots: []domain.Snapshot{targetSnapshot, ownerSnapshot}}
			event := birth
			event.ID, event.Branch = strings.Repeat("e", 32), "local-feature"
			prepared, err := f.svc.PrepareTrackingAttachment(ctx, event, observed, []string{event.GitAfter})
			if err != nil {
				t.Fatalf("clean preparation failed: %v", err)
			}
			if prepared.Event.MemoryHash != pin || prepared.Event.MemorySource != owner || !prepared.Event.MemoryPinned {
				t.Fatalf("app did not normalize exact supported source owner: %+v", prepared.Event)
			}
			if mode == "implicit-owner-control" {
				return
			}
			// The shape produced by a selected position: old and chosen context
			// equal Target, with the independently proven owner explicit.
			local := prepared.Event
			local.ID, local.Kind = strings.Repeat("d", 32), "position"
			local.Source, local.Target = birth.Target, birth.Target
			if _, err := f.svc.ValidateHistorySource(ctx, local); err != nil {
				t.Fatalf("normalized local evidence invalid: %v", err)
			}
			f.history.events = []domain.HistoryEvent{local}
			_, err = f.svc.PrepareTrackingAttachment(ctx, event, observed, []string{event.GitAfter})
			if err != nil {
				t.Fatalf("equivalent normalized owner rejected (conflict=%v): remote MemorySource=%q Source=%s Target=%s; local MemorySource=%s, same memory=%s: %v", errors.Is(err, domain.ErrSyncConflict), birth.MemorySource, birth.Source, birth.Target, local.MemorySource, pin, err)
			}
		})
	}
}

func TestTrackingPRWitnessExplicitMemoryOwner(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "contradictory"}[wrong], func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			ctx := context.Background()
			source := f.observation(f.memory(f.receipt.Source, "", "PR pin"))
			source.Kind = "birth"
			if wrong {
				source.MemorySource = f.snapshot(source.RepoID, "wrong explicit owner")
			}
			birth := source
			birth.ID, birth.Branch, birth.BranchID = strings.Repeat("a", 32), f.receipt.Branch, f.receipt.BranchID
			birth.GitAfter, birth.MemoryHash, birth.MemorySource = strings.Repeat("c", 40), "", ""
			receipt := f.receipt
			receipt.Target = source.Target
			ref := domain.Ref{RepoID: birth.RepoID, Kind: domain.RefBranch, Name: birth.Branch, BranchID: birth.BranchID, Target: source.Target}
			snap, err := f.store.GetSnapshot(ctx, source.Target)
			if err != nil {
				t.Fatal(err)
			}
			event := birth
			event.ID, event.Branch, event.GitAfter = strings.Repeat("e", 32), "local-task", receipt.PR.MergeSHA
			got, err := f.svc.PrepareTrackingAttachment(ctx, event, inbound.RemoteBranchObservation{Ref: ref, History: []domain.HistoryEvent{birth, source, receipt}, Snapshots: []domain.Snapshot{snap}}, []string{event.GitAfter})
			if wrong {
				if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("contradictory PR source owner accepted: %v", err)
				}
			} else if err != nil || got.Event.MemoryHash != source.MemoryHash {
				t.Fatalf("valid PR source pin rejected: %v", err)
			}
		})
	}
}
