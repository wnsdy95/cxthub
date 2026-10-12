package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func freezeRawCoverageDelivery(t *testing.T) (string, *Container, *mergeDeliverySync, string) {
	t.Helper()
	root, container, syncer := newMergeDeliveryFixture(t)
	syncer.appendErr = errors.New("offline")
	handleIncomingContexts(context.Background(), container, root)
	key, job := mergeDeliveryJob(t, root)
	if job.Completed || len(job.Candidates) != 1 {
		t.Fatalf("delivery not frozen: %+v", job)
	}
	job.LastAttempt = time.Now().Add(-2 * time.Minute)
	if err := writeMergeDelivery(root, key, job); err != nil {
		t.Fatal(err)
	}
	syncer.appendErr = nil
	return root, container, syncer, key
}

func TestMergeDeliveryRawCoverageDoesNotAcknowledgeExcludedSource(t *testing.T) {
	for _, state := range []string{"not_selected", "review"} {
		for _, relationship := range []string{"exact source", "source ancestor"} {
			t.Run(state+"/"+relationship, func(t *testing.T) {
				root, container, syncer, key := freezeRawCoverageDelivery(t)
				candidate, base := syncer.snapshots[0].ID, syncer.ref.Target
				source := candidate
				if relationship == "source ancestor" {
					source = briefingHash("excluded PR source")
					syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: source, RepoID: syncer.ref.RepoID, Parents: []domain.ContentHash{candidate}})
				}
				syncer.snapshots[1].GraftParents = []domain.ContentHash{source}
				semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
				semantic.view.Inclusion.Merges = []domain.BranchContextMerge{{Source: source, DestinationBranchID: syncer.ref.BranchID, State: state}}
				container.Sync = semantic
				attempts := len(syncer.appends)
				_, reflected, attempted := replayMergeDeliveriesAt(context.Background(), container, root, true)
				job, err := readMergeDelivery(root, key)
				if err != nil || !attempted || reflected || job.Completed || job.Reason == "" {
					t.Fatalf("raw ancestry certified exclusion: job=%+v attempted=%v reflected=%v err=%v", job, attempted, reflected, err)
				}
				if len(syncer.appends) != attempts || syncer.ref.Target != base || !snapshotContains(syncer.snapshots, base, candidate) {
					t.Fatalf("excluded raw coverage changed: ref=%+v appends=%v", syncer.ref, syncer.appends)
				}
			})
		}
	}
}

func TestMergeDeliveryRawCoverageCannotOverrideFinalSemanticQuery(t *testing.T) {
	root, container, syncer, key := freezeRawCoverageDelivery(t)
	candidate, base := syncer.snapshots[0].ID, syncer.ref.Target
	source := briefingHash("excluded PR source")
	syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: source, RepoID: syncer.ref.RepoID, Parents: []domain.ContentHash{candidate}})
	syncer.snapshots[1].GraftParents = []domain.ContentHash{source}
	excluded := semanticMergeView(syncer)
	excluded.Inclusion.Merges = []domain.BranchContextMerge{{Source: source, DestinationBranchID: syncer.ref.BranchID, State: "not_selected"}}
	semantic := &semanticMergeSync{mergeDeliverySync: syncer}
	queries := 0
	semantic.onQuery = func() {
		queries++
		semantic.view = excluded
		if queries == 2 {
			semantic.view = semanticMergeView(syncer, candidate)
		}
	}
	container.Sync = semantic
	attempts := len(syncer.appends)
	_, _, attempted := replayMergeDeliveriesAt(context.Background(), container, root, true)
	job, err := readMergeDelivery(root, key)
	if err != nil || !attempted || queries != 3 || job.Completed || job.Reason == "" {
		t.Fatalf("final semantic omission ignored: job=%+v queries=%d attempted=%v err=%v", job, queries, attempted, err)
	}
	if len(syncer.appends) != attempts || syncer.ref.Target != base {
		t.Fatalf("read-only coverage changed ref: ref=%+v appends=%v", syncer.ref, syncer.appends)
	}
}

func TestAppendMergeCandidatesRawPruningCannotBypassSemanticExclusion(t *testing.T) {
	for _, state := range []string{"not_selected", "review"} {
		t.Run(state, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			candidate, base := syncer.snapshots[0].ID, syncer.ref.Target
			newer := briefingHash("newer candidate")
			syncer.snapshots = append(syncer.snapshots, domain.Snapshot{ID: newer, RepoID: syncer.ref.RepoID, GraftParents: []domain.ContentHash{candidate}})
			semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer, newer)}
			semantic.view.Inclusion.Merges = []domain.BranchContextMerge{{Source: candidate, DestinationBranchID: syncer.ref.BranchID, State: state}}
			container.Sync = semantic
			candidates := []domain.Snapshot{{ID: candidate}, {ID: newer}}
			input := inbound.SyncInput{Cwd: root, RepoID: syncer.ref.RepoID, ExpectedBranchID: syncer.ref.BranchID}
			reflected, err := appendMergeCandidates(context.Background(), container, syncer.ref.Name, candidates, input, gitOut(root, "rev-parse", "HEAD"), nil)
			if !errors.Is(err, domain.ErrSelectionChanged) || reflected || len(syncer.appends) != 0 || syncer.ref.Target != base {
				t.Fatalf("raw candidate pruning bypassed query: reflected=%v err=%v appends=%v ref=%+v", reflected, err, syncer.appends, syncer.ref)
			}
		})
	}
}

func TestAppendMergeCandidatesRawCoverageRequiresAvailableQuery(t *testing.T) {
	root, container, syncer := newMergeDeliveryFixture(t)
	candidate := syncer.snapshots[0].ID
	syncer.snapshots[1].GraftParents = []domain.ContentHash{candidate}
	unavailable := errors.New("context query unavailable")
	container.Sync = &semanticMergeSync{mergeDeliverySync: syncer, err: unavailable}
	input := inbound.SyncInput{Cwd: root, RepoID: syncer.ref.RepoID, ExpectedBranchID: syncer.ref.BranchID}
	reflected, err := appendMergeCandidates(context.Background(), container, syncer.ref.Name, []domain.Snapshot{{ID: candidate}}, input, gitOut(root, "rev-parse", "HEAD"), nil)
	if !errors.Is(err, unavailable) || reflected || len(syncer.appends) != 0 {
		t.Fatalf("raw coverage bypassed unavailable query: reflected=%v err=%v appends=%v", reflected, err, syncer.appends)
	}
}

func TestAppendMergeCandidatesNonFastForwardRequiresSemanticCoverage(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "legacy"
		if query {
			name = "authoritative"
		}
		t.Run(name, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			nonFastForward := &backendclient.HTTPError{Status: 409, Code: "non_fast_forward"}
			syncer.appendErr = nonFastForward
			if query {
				semantic := &semanticMergeSync{mergeDeliverySync: syncer, view: semanticMergeView(syncer)}
				semantic.view.Inclusion.CodeCommit = gitOut(root, "rev-parse", "HEAD")
				container.Sync = semantic
			}
			input := inbound.SyncInput{Cwd: root, RepoID: syncer.ref.RepoID, ExpectedBranchID: syncer.ref.BranchID}
			reflected, err := appendMergeCandidates(context.Background(), container, syncer.ref.Name, syncer.snapshots[:1], input, gitOut(root, "rev-parse", "HEAD"), nil)
			if query && (!errors.Is(err, nonFastForward) || reflected) {
				t.Fatalf("non-fast-forward certified semantic coverage: reflected=%v err=%v", reflected, err)
			}
			if !query && (err != nil || !reflected) {
				t.Fatalf("legacy non-fast-forward changed: reflected=%v err=%v", reflected, err)
			}
			if len(syncer.appends) != 1 {
				t.Fatalf("append attempts=%v", syncer.appends)
			}
		})
	}
}

func TestAppendMergeCandidatesRawCoveragePreservesLegacyCallers(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "no reader"
		if query {
			name = "unidentified reader"
		}
		t.Run(name, func(t *testing.T) {
			root, container, syncer := newMergeDeliveryFixture(t)
			candidate := syncer.snapshots[0].ID
			syncer.snapshots[1].GraftParents = []domain.ContentHash{candidate}
			input := inbound.SyncInput{Cwd: root, RepoID: syncer.ref.RepoID, ExpectedBranchID: syncer.ref.BranchID}
			if query {
				input.ExpectedBranchID = ""
				container.Sync = &semanticMergeSync{mergeDeliverySync: syncer, err: errors.New("query must not run")}
			}
			reflected, err := appendMergeCandidates(context.Background(), container, syncer.ref.Name, []domain.Snapshot{{ID: candidate}}, input, "", nil)
			if err != nil || !reflected || len(syncer.appends) != 0 {
				t.Fatalf("legacy raw coverage changed: reflected=%v err=%v appends=%v", reflected, err, syncer.appends)
			}
		})
	}
}
