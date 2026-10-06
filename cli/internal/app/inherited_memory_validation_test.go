package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type inheritedMemoryReadProbe struct {
	outbound.SessionStore
	corrupt domain.ContentHash
	reads   map[domain.ContentHash]int
}

func (s *inheritedMemoryReadProbe) GetMemory(ctx context.Context, h domain.ContentHash) (domain.MemoryDigest, error) {
	s.reads[h]++
	d, err := s.SessionStore.GetMemory(ctx, h)
	if h == s.corrupt {
		d.Summary += " corrupt"
	}
	return d, err
}

func TestInheritedMemorySelectionValidatesBeforeFiltering(t *testing.T) {
	for _, mode := range []string{"valid chain", "missing ancestor", "foreign ancestor", "corrupt ancestor", "wrong explicit owner", "foreign repository owner", "missing witness", "publish witness", "nonroot successor", "foreign successor owner", "divergent roots", "unlinked empty", "immutable ID collision"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newPRPositionMemoryFixture(t)
			f.receipt.SourceBranchID = domain.LegacyContextBranchID(f.receipt.RepoID, "feature")
			owner := f.snapshot(f.receipt.RepoID, "owner")
			if mode == "foreign repository owner" {
				owner = f.snapshot(string(domain.HashContent([]byte("different repo"))), "foreign")
			}
			ancestor := f.memory(owner, "", "ancestor")
			previous := ancestor
			if mode == "missing ancestor" {
				previous = domain.HashContent([]byte("missing ancestor"))
			}
			if mode == "foreign ancestor" {
				previous = f.memory(f.receipt.Source, "", "wrong chain owner")
			}
			ma := f.memory(owner, previous, "inherited descendant")
			before := f.observation(ma)
			before.MemorySource = owner
			f.history.events = append(f.history.events, before)
			mb := f.memory(f.receipt.Source, "", "first own root")
			after := f.observation(mb)
			after.MemorySelectionParent = before.ID
			if mode == "wrong explicit owner" {
				before.MemorySource = f.snapshot(f.receipt.RepoID, "wrong owner")
			}
			if mode == "publish witness" {
				before.Kind = "publish"
			}
			if mode == "nonroot successor" {
				after.MemoryHash = f.memory(f.receipt.Source, mb, "nonroot")
			}
			if mode == "foreign successor owner" {
				after.MemoryHash = f.memory(owner, "", "wrong successor")
			}
			events := []domain.HistoryEvent{before, after}
			switch mode {
			case "missing witness":
				events = events[1:]
			case "divergent roots":
				other := after
				other.ID = strings.Repeat("3", 32)
				other.MemoryHash = f.memory(f.receipt.Source, "", "sibling root")
				events = append(events, other)
			case "unlinked empty":
				empty := before
				empty.ID = strings.Repeat("4", 32)
				empty.MemoryHash = ""
				empty.MemorySource = ""
				events = append(events, empty)
			case "immutable ID collision":
				collision := before
				collision.MemoryHash = mb
				events = append(events, collision)
			}
			probe := &inheritedMemoryReadProbe{SessionStore: f.store, reads: map[domain.ContentHash]int{}}
			if mode == "corrupt ancestor" {
				probe.corrupt = ancestor
			}
			svc := NewContextHistoryService(probe, f.history)
			frozen := append([]domain.HistoryEvent(nil), events...)
			got, err := svc.ResolvePRSourcePositionFromHistory(ctx, f.receipt, events)
			if mode == "valid chain" {
				if err != nil || got.MemoryHash != mb || got.MemorySource != "" {
					t.Fatalf("valid chain failed: %+v %v", got, err)
				}
				if probe.reads[ancestor] == 0 {
					t.Fatal("superseded predecessor ancestry not read")
				}
			} else if err == nil {
				t.Fatal("invalid/unordered evidence accepted")
			}
			// Tracking reaches the shared filter before per-source validation.
			f.history.events = nil
			snap, snapErr := f.store.GetSnapshot(ctx, f.receipt.Source)
			if snapErr != nil {
				t.Fatal(snapErr)
			}
			ref := domain.Ref{RepoID: f.receipt.RepoID, Kind: domain.RefBranch, Name: "feature", BranchID: f.receipt.SourceBranchID, Target: f.receipt.Source}
			input := before
			input.ID = strings.Repeat("9", 32)
			input.Kind = "attach"
			attachment, trackingErr := svc.PrepareTrackingAttachment(ctx, input, inbound.RemoteBranchObservation{Ref: ref, History: events, Snapshots: []domain.Snapshot{snap}}, []string{before.GitAfter})
			if mode == "valid chain" {
				if trackingErr != nil || attachment.Event.MemoryHash != mb || attachment.Event.MemorySource != "" {
					t.Fatalf("tracking valid predecessor: %v", trackingErr)
				}
			} else if trackingErr == nil {
				t.Fatal("tracking filtered invalid/unordered evidence")
			}
			// Rewrite projects sources, so it retains unlinked empty/sibling
			// observations; its caller still decides their compatibility.
			filtered, filterErr := svc.RewriteHistorySources(ctx, events, before.BranchID, before.WorktreeID)
			retained := mode == "valid chain" || mode == "unlinked empty" || mode == "divergent roots"
			if retained && filterErr != nil {
				t.Fatalf("valid source projection failed: %v", filterErr)
			}
			if !retained && filterErr == nil {
				t.Fatal("rewrite filtered invalid predecessor evidence")
			}
			if mode == "valid chain" && (len(filtered) != 1 || !reflect.DeepEqual(filtered[0], after)) {
				t.Fatal("did not remove only exact predecessor")
			}
			if (mode == "unlinked empty" || mode == "divergent roots") && len(filtered) != 2 {
				t.Fatal("unlinked observation erased")
			}
			if !reflect.DeepEqual(events, frozen) {
				t.Fatal("raw proof mutated")
			}
		})
	}
}
