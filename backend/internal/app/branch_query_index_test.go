package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestBranchQueryIndexMatchesHistorySelection(t *testing.T) {
	fixture, main, history := branchContextFixture(t)
	ctx := systemTestContext()
	child := main
	child.Name, child.BranchID = "child", "child-identity"
	history = append(history,
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 901), RepoID: string(fixture.repo), Kind: "publish", Branch: main.Name, BranchID: main.BranchID, Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)},
		domain.HistoryEvent{ID: fmt.Sprintf("%032x", 902), RepoID: string(fixture.repo), Kind: "birth", Branch: child.Name, BranchID: child.BranchID, Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)},
	)
	for extra := range 2048 {
		history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("%032x", extra+10000), RepoID: string(fixture.repo), Branch: "other", BranchID: "other-identity", Kind: "position", Target: main.Target, GitAfter: effectiveOID(99)})
	}
	snapshots, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(history)
		}
		inputs, err := newBranchQueryInputs(ctx, fixture.repo, snapshots, history)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range []domain.Ref{main, child} {
			rows := inputs.History().TargetEvents(ref.BranchID, ref.Target)
			if got, want := branchCodeCandidates(ref, rows), branchCodeCandidates(ref, history); !reflect.DeepEqual(got, want) {
				t.Fatalf("indexed candidates = %v, want %v", got, want)
			}
			for _, position := range inputs.History().ReceiptPositions(main.BranchID) {
				receipt := history[position]
				_, got, err := inputs.History().Completion(receipt)
				want, wantErr := hasPRCompletion(history, receipt)
				if got != want || !errors.Is(err, wantErr) {
					t.Fatalf("indexed completion = %v/%v, want %v/%v", got, err, want, wantErr)
				}
			}
			for _, selected := range []struct{ code, state string }{{effectiveOID(3), "included"}, {effectiveOID(1), "not_selected"}, {effectiveOID(99), "review"}} {
				evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
				if err != nil {
					t.Fatal(err)
				}
				inclusion, err := fixture.svc.branchContextFromInputs(ctx, ref, selected.code, inputs, evidence)
				if err != nil || len(inclusion.Merges) != 2 {
					t.Fatalf("indexed inherited selection = %+v, %v", inclusion, err)
				}
				for _, merge := range inclusion.Merges {
					if !merge.DeliveryAcknowledged || merge.State != selected.state || merge.DestinationBranchID != main.BranchID {
						t.Fatalf("historical integration changed: %+v", merge)
					}
				}
			}
		}
		reused := child
		reused.BranchID = "recreated-child"
		evidence, _ := fixture.svc.newCodeEvidence(ctx, fixture.repo)
		inclusion, err := fixture.svc.branchContextFromInputs(ctx, reused, effectiveOID(3), inputs, evidence)
		if err != nil || len(inclusion.Merges) != 0 {
			t.Fatalf("same-name recreated branch inherited history: %+v %v", inclusion, err)
		}
	}
}

func TestBranchQueryIndexRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	repo, foreign := hh("index-repo"), hh("foreign-index-repo")
	if _, err := newBranchHistoryIndex(ctx, repo, []domain.HistoryEvent{{RepoID: string(foreign)}}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("foreign history accepted: %v", err)
	}
	snapshot := domain.Snapshot{ID: hh("index-snapshot"), RepoID: repo}
	for _, snapshots := range [][]domain.Snapshot{{{ID: snapshot.ID, RepoID: foreign}}, {snapshot, snapshot}} {
		if _, err := newBranchQueryInputs(ctx, repo, snapshots, nil); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("invalid snapshots accepted: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := newBranchQueryInputs(canceled, repo, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	index, err := newBranchHistoryIndex(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvedBranchCodeFromIndex(ctx, domain.Ref{RepoID: foreign}, index, nil); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("foreign index reused: %v", err)
	}
}

func TestBranchQueryIndexDoesNotRetainPriorRequest(t *testing.T) {
	fixture, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	before, err := newBranchHistoryIndex(ctx, fixture.repo, history[:1])
	if err != nil {
		t.Fatal(err)
	}
	after, err := newBranchHistoryIndex(ctx, fixture.repo, history)
	if err != nil {
		t.Fatal(err)
	}
	if _, complete, err := before.Completion(history[0]); complete || err != nil {
		t.Fatalf("future completion leaked backwards: %v %v", complete, err)
	}
	if _, complete, err := after.Completion(history[0]); !complete || err != nil {
		t.Fatalf("new completion hidden: %v %v", complete, err)
	}
	conflict := slices.Clone(history)
	conflict[1].Source = ref.Target
	broken, err := newBranchHistoryIndex(ctx, fixture.repo, conflict)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := broken.Completion(history[0]); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("conflicting completion accepted: %v", err)
	}
}
