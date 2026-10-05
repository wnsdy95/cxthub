package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestRecordedSelfOwnedPinPRSpelling(t *testing.T) {
	for _, mode := range []string{"implicit-control", "explicit-control", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			ctx := context.Background()
			memory := f.memory(f.receipt.Source, "", "same self-owned immutable digest")
			implicit := f.observation(memory)
			explicit := implicit
			explicit.ID, explicit.MemorySource = strings.Repeat("2", 32), implicit.Target
			for _, e := range []domain.HistoryEvent{implicit, explicit} {
				got, err := f.svc.ValidateHistorySource(ctx, e)
				if err != nil || !reflect.DeepEqual(got, e) {
					t.Fatalf("supported raw pin failed precondition: explicit=%t err=%v", e.MemorySource != "", err)
				}
			}
			events := []domain.HistoryEvent{implicit, explicit}
			if mode == "implicit-control" {
				events = events[:1]
			}
			if mode == "explicit-control" {
				events = events[1:]
			}
			before := append([]domain.HistoryEvent(nil), events...)
			got, err := f.svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, events)
			if !reflect.DeepEqual(before, events) {
				t.Fatal("resolver rewrote immutable supplied events")
			}
			if err != nil || got.MemoryHash != memory || got.Snapshot != implicit.Target || !got.MemoryPinned {
				t.Fatalf("equivalent self-owner spelling rejected by actual PR resolver: mode=%s err=%v", mode, err)
			}
		})
	}
}

func TestRecordedSelfOwnedPinTrackingSpelling(t *testing.T) {
	for _, mode := range []string{"implicit-control", "explicit-control", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			ctx := context.Background()
			memory := f.memory(f.receipt.Source, "", "same self-owned immutable digest")
			birth := f.observation(memory)
			birth.Kind = "birth"
			position := birth
			position.ID, position.Kind, position.MemorySource = strings.Repeat("2", 32), "position", birth.Target
			events := []domain.HistoryEvent{birth, position}
			if mode == "implicit-control" {
				events = events[:1]
			}
			if mode == "explicit-control" {
				birth.MemorySource = birth.Target
				events = []domain.HistoryEvent{birth}
			}
			for _, e := range events {
				if _, err := f.svc.ValidateHistorySource(ctx, e); err != nil {
					t.Fatalf("invalid fixture pin: %v", err)
				}
			}
			snap, err := f.store.GetSnapshot(ctx, birth.Target)
			if err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{RepoID: birth.RepoID, Kind: domain.RefBranch, Name: birth.Branch, BranchID: birth.BranchID, Target: birth.Target}
			event := birth
			event.ID, event.Branch = strings.Repeat("e", 32), "local-feature"
			got, err := f.svc.PrepareTrackingAttachment(ctx, event, inbound.RemoteBranchObservation{Ref: ref, History: events, Snapshots: []domain.Snapshot{snap}}, []string{event.GitAfter})
			if err != nil || got.Event.MemoryHash != memory || got.Event.Target != birth.Target || !got.Event.MemoryPinned {
				t.Fatalf("equivalent self-owner spelling rejected by actual tracking preparation: mode=%s err=%v", mode, err)
			}
		})
	}
}

func TestRecordedSelfOwnedPinProofNormalization(t *testing.T) {
	for _, explicitRaw := range []bool{false, true} {
		name := "implicit-proof-normalized-explicit"
		if explicitRaw {
			name = "explicit-proof-normalized-implicit"
		}
		t.Run(name, func(t *testing.T) {
			f := newPRPositionMemoryFixture(t)
			ctx := context.Background()
			memory := f.memory(f.receipt.Source, "", "target-owned memory")
			birth := f.observation(memory)
			birth.Kind = "birth"
			// Source is a previous context, not this digest's Target owner.
			birth.Source = f.snapshot(birth.RepoID, "different previous source")
			if explicitRaw {
				birth.MemorySource = birth.Target
			}
			if _, err := f.svc.ValidateHistorySource(ctx, birth); err != nil {
				t.Fatalf("invalid supported raw evidence: %v", err)
			}
			snap, err := f.store.GetSnapshot(ctx, birth.Target)
			if err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{RepoID: birth.RepoID, Kind: domain.RefBranch, Name: birth.Branch, BranchID: birth.BranchID, Target: birth.Target}
			event := birth
			event.ID, event.Branch = strings.Repeat("e", 32), "local-feature"
			prepared, err := f.svc.PrepareTrackingAttachment(ctx, event, inbound.RemoteBranchObservation{Ref: ref, History: []domain.HistoryEvent{birth}, Snapshots: []domain.Snapshot{snap}}, []string{event.GitAfter})
			if err != nil {
				t.Fatalf("unmodified supported preparation failed: %v", err)
			}
			// Only normalize the new attachment; retain the raw proof payload and ID.
			if explicitRaw {
				prepared.Event.MemorySource = ""
			} else {
				prepared.Event.MemorySource = birth.Target
			}
			if _, err := f.svc.ValidateHistorySource(ctx, prepared.Event); err != nil {
				t.Fatalf("equivalent normalized event invalid: %v", err)
			}
			if err := domain.ValidateTrackingAttachment(prepared); err != nil {
				t.Fatalf("exact-proof matcher rejects equivalent self-owner normalization: %v", err)
			}
		})
	}
}
