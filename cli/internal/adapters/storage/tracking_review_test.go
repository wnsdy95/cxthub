package storage

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"reflect"
	"strings"
	"testing"
)

func TestTrackingRejectsLegacyReleaseBeforeLifecycleRefWrite(t *testing.T) {
	for _, kind := range []string{"archive", "rename"} {
		t.Run(kind, func(t *testing.T) {
			f := newTrackingFixture(t)
			a := &f.commit.Attachment
			id := domain.LegacyContextBranchID(a.Event.RepoID, a.Event.Branch)
			a.Event.BranchID, a.ObservedRef.BranchID = id, id
			f.commit.Position.Next.BranchID = id
			f.commit.Position.Next.Selection.BranchID = id
			first := a.Proof[0]
			first.Kind, first.BranchID, first.Branch, first.PreviousBranch = "rename", id, "temporary", a.Event.Branch
			again := first
			again.ID, again.Branch, again.PreviousBranch, again.BindingParent, again.NameParent = strings.Repeat("a", 32), a.Event.Branch, "temporary", first.ID, first.ID
			pin := a.Proof[1]
			pin.BranchID = id
			a.Proof = []domain.HistoryEvent{first, again, pin}
			for _, e := range a.Proof {
				if err := f.store.PutHistoryEvent(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
			ref := a.ObservedRef
			if err := f.store.PutRef(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			f.commit.ExpectedRef = &ref
			release := again
			release.ID, release.Kind, release.BindingParent, release.NameParent, release.PreviousBranch = strings.Repeat("b", 32), kind, again.ID, "", ""
			if kind == "rename" {
				release.PreviousBranch, release.Branch = release.Branch, "elsewhere"
			}
			// Durable lifecycle history exists, but legacy ref/tag still appears active.
			if err := f.store.PutHistoryEvent(context.Background(), release); err != nil {
				t.Fatal(err)
			}
			before := trackingMutableFiles(t, f.store)
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("accepted released legacy branch: %v", err)
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("failed attachment mutated state")
			}
		})
	}
}

func TestTrackingPRWitnessDoesNotCreateOrResurrectSourceBranch(t *testing.T) {
	for _, reused := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh replica", true: "source name reused"}[reused], func(t *testing.T) {
			f := newTrackingFixture(t)
			ctx := context.Background()
			a := &f.commit.Attachment
			source := a.Proof[0]
			source.ID, source.BranchID, source.Branch, source.GitAfter = strings.Repeat("5", 32), "source-X", "source", strings.Repeat("b", 40)
			receipt := a.Proof[1]
			receipt.ID, receipt.Kind, receipt.SourceBranchID, receipt.PRCompleted = strings.Repeat("6", 32), "pr-merge", source.BranchID, true
			receipt.PR = &domain.PullRequestMerge{Number: 1, BaseBranch: a.Event.Branch, HeadBranch: source.Branch, HeadSHA: source.GitAfter, MergeSHA: a.Code}
			archive := source
			archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("7", 32), "archive", source.ID
			replacement := source
			replacement.ID, replacement.BranchID, replacement.BindingParent = strings.Repeat("8", 32), "source-Y", archive.ID
			all := append(append([]domain.HistoryEvent{}, a.Proof...), source, receipt, archive, replacement)
			proof, err := domain.TrackingProof(a.ObservedRef, all, a.Code, a.Event.Target)
			if err != nil {
				t.Fatal(err)
			}
			a.Proof = proof
			if reused {
				for _, e := range []domain.HistoryEvent{source, archive, replacement} {
					if err := f.store.PutHistoryEvent(ctx, e); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := f.store.ListHistoryEvents(ctx, a.Event.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.CommitTrackingAttachment(ctx, f.commit); err != nil {
				t.Fatal(err)
			}
			after, err := f.store.ListHistoryEvents(ctx, a.Event.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			foreign := func(events []domain.HistoryEvent) []domain.HistoryEvent {
				var out []domain.HistoryEvent
				for _, e := range events {
					if e.BranchID == source.BranchID || e.BranchID == replacement.BranchID {
						out = append(out, e)
					}
				}
				return out
			}
			if !reflect.DeepEqual(foreign(before), foreign(after)) {
				t.Fatal("tracking R changed source branch history")
			}
			state, err := domain.ProjectContextBranches(after)
			if err != nil {
				t.Fatal(err)
			}
			if reused {
				if state.Active[source.Branch].ID != replacement.BranchID {
					t.Fatal("resurrected old PR source")
				}
			} else if _, ok := state.Active[source.Branch]; ok {
				t.Fatal("invented active source branch from historical witness")
			}
		})
	}
}
