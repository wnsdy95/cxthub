package storage

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestTrackingAttachmentVerifiedMemoryOwnerEquivalence(t *testing.T) {
	for _, mode := range []string{"source-owner", "target-owner", "recovery", "invalid-owner", "same-ID-payload", "explicit-source-mismatch", "explicit-target-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newTrackingFixture(t)
			ctx := context.Background()
			owner := f.old
			if mode == "target-owner" || mode == "explicit-target-mismatch" {
				owner = f.chosen
			}
			actualOwner := owner
			if mode == "invalid-owner" {
				actualOwner = f.shared // neither named source, target nor explicit owner
			}
			hash, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: actualOwner, Summary: "exact inherited pin"})
			if err != nil {
				t.Fatal(err)
			}
			for i := range f.commit.Attachment.Proof {
				p := &f.commit.Attachment.Proof[i]
				p.Source, p.MemorySource, p.MemoryHash = f.old, "", hash
			}
			f.commit.Attachment.Event.MemoryHash = hash
			f.commit.Attachment.Event.MemorySource = owner
			if mode == "target-owner" || mode == "explicit-target-mismatch" {
				f.commit.Attachment.Event.MemorySource = ""
			}
			p := &f.commit.Position.Next
			p.MemoryHash, p.MemorySource = hash, f.commit.Attachment.Event.MemorySource
			p.Selection.MemoryHash, p.Selection.MemorySource = p.MemoryHash, p.MemorySource
			local := f.commit.Attachment.Event
			local.ID, local.Kind, local.MemorySource = strings.Repeat("5", 32), "position", owner
			if mode == "explicit-source-mismatch" || mode == "explicit-target-mismatch" {
				// A matching Source/Target must not excuse contradictory explicit provenance.
				local.Source, local.MemorySource = f.old, f.shared
			}
			if mode == "same-ID-payload" {
				local = f.commit.Attachment.Proof[1]
				local.MemorySource = owner // semantically same pin, different immutable payload
			}
			if err := f.store.PutHistoryEvent(ctx, local); err != nil {
				t.Fatal(err)
			}
			before := trackingMutableFiles(t, f.store)
			if mode == "recovery" {
				f.prefix(t, f.journal(t), 6)
				err = f.peer.PutRef(ctx, domain.Ref{RepoID: local.RepoID, Kind: domain.RefTag, Name: "after-owner-recovery", Target: f.old})
			} else {
				err = f.store.CommitTrackingAttachment(ctx, f.commit)
			}
			if mode == "invalid-owner" || mode == "same-ID-payload" || mode == "explicit-source-mismatch" || mode == "explicit-target-mismatch" {
				if err == nil || !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
					t.Fatalf("invalid evidence accepted or mutated state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("equivalent verified owner rejected: %v", err)
			}
			f.applied(t)
			events, err := f.store.ListHistoryEvents(ctx, local.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			for _, proof := range f.commit.Attachment.Proof {
				for _, got := range events {
					if got.ID == proof.ID && !reflect.DeepEqual(got, proof) {
						t.Fatal("owner comparison rewrote immutable proof")
					}
				}
			}
		})
	}
}

func TestTrackingPRWitnessExplicitMemoryOwner(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "contradictory"}[wrong], func(t *testing.T) {
			f := newTrackingFixture(t)
			a := &f.commit.Attachment
			source := a.Proof[0]
			source.ID, source.BranchID, source.Branch, source.GitAfter = strings.Repeat("5", 32), "source-X", "source", strings.Repeat("b", 40)
			if wrong {
				source.MemorySource = f.old // digest belongs to Target, so explicit owner must not be ignored
			}
			receipt := a.Proof[1]
			receipt.ID, receipt.Kind, receipt.SourceBranchID, receipt.PRCompleted = strings.Repeat("6", 32), "pr-merge", source.BranchID, true
			receipt.PR = &domain.PullRequestMerge{Number: 1, BaseBranch: a.Event.Branch, HeadBranch: source.Branch, HeadSHA: source.GitAfter, MergeSHA: a.Code}
			var err error
			a.Proof, err = domain.TrackingProof(a.ObservedRef, append(a.Proof, source, receipt), a.Code, a.Event.Target)
			if err != nil {
				t.Fatal(err)
			}
			before := trackingMutableFiles(t, f.store)
			err = f.store.CommitTrackingAttachment(context.Background(), f.commit)
			if wrong {
				if err == nil || !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
					t.Fatalf("contradictory PR witness accepted or mutated state: %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid PR witness rejected: %v", err)
			}
		})
	}
}
