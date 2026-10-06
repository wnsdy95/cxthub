package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// This optional method compiles against the baseline too: the RED exercises
// the real hook ignoring metadata discovery and invoking an unscoped fetch.
type incomingMetadataSync struct {
	fixedBriefingSync
	catalog      []domain.Snapshot
	discoveryErr error
	inputs       []inbound.SyncInput
	discovered   bool
	allowPull    bool
	legacy       bool
	onDiscovery  func()
}

func (s *incomingMetadataSync) DiscoverIncomingSnapshots(context.Context, inbound.SyncInput) ([]domain.Snapshot, bool, error) {
	s.discovered = true
	if s.onDiscovery != nil {
		s.onDiscovery()
	}
	return s.catalog, !s.legacy, s.discoveryErr
}

type queuedIncomingMetadata struct {
	*incomingMetadataSync
	queued []outbound.MergedPullRequest
}

func (s *queuedIncomingMetadata) QueuePullRequest(_ context.Context, _ inbound.SyncInput, pr outbound.MergedPullRequest) error {
	s.queued = append(s.queued, pr)
	return nil
}

func TestIncomingMetadataFailureKeepsDurablePRHandoffBeforeDiscovery(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &queuedIncomingMetadata{incomingMetadataSync: &incomingMetadataSync{discoveryErr: context.Canceled}}
	s.onDiscovery = func() {
		if len(s.queued) != 1 {
			t.Fatal("metadata discovery preceded durable PR handoff")
		}
		cancel()
	}
	resolver := &fakePRMergeResolver{pulls: []outbound.MergedPullRequest{{Number: 423, BaseBranch: "main", HeadBranch: "feature/other"}}}
	handleIncomingContexts(ctx, &Container{Sync: s, PRMerges: resolver}, root)
	if !s.discovered || len(s.inputs) != 0 || len(s.queued) != 1 {
		t.Fatalf("discovered=%v transfers=%d queued=%d", s.discovered, len(s.inputs), len(s.queued))
	}
}

func TestIncomingMetadataLegacyCapabilityKeepsCompleteTransfer(t *testing.T) {
	s := &incomingMetadataSync{legacy: true, allowPull: true}
	if _, err := fetchIncomingContexts(context.Background(), &Container{Sync: s}, t.TempDir(), "main", nil); err != nil {
		t.Fatal(err)
	}
	if len(s.inputs) != 1 || s.inputs[0].Ref != "" || s.inputs[0].RequireBranchPlan || !s.inputs[0].FetchOnly {
		t.Fatalf("legacy behavior changed: %+v", s.inputs)
	}
}
func (s *incomingMetadataSync) Pull(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.inputs = append(s.inputs, in)
	if s.allowPull {
		return inbound.SyncOutput{Pulled: len(in.ObservationRoots)}, nil
	}
	// Stop before promotion; the request boundary is the regression under test.
	return inbound.SyncOutput{}, errors.New("synthetic transfer stop")
}

func TestIncomingMetadataHookSelectsOtherBranchAndLocalCandidates(t *testing.T) {
	root := newPostMergeBriefingRepo(t)
	shas := incomingCommitSHAs(root)
	if len(shas) != 1 {
		t.Fatalf("fixture Git range %v", shas)
	}
	old := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := saveRewrites(root, map[string]string{old: shas[0]}); err != nil {
		t.Fatal(err)
	}
	id := briefingHash("remote other branch candidate")
	syncer := &incomingMetadataSync{catalog: []domain.Snapshot{{ID: id, Branch: "feature/other", Message: "incoming [git " + old + "]"}}}
	handleIncomingContexts(context.Background(), &Container{Sync: syncer}, root)
	if !syncer.discovered || len(syncer.inputs) != 1 || syncer.inputs[0].Ref != "main" || !syncer.inputs[0].FetchOnly || !reflect.DeepEqual(syncer.inputs[0].ObservationRoots, []domain.ContentHash{id}) {
		t.Fatalf("metadata discovery=%v, fetches=%+v; want selected main with aliased other-branch root", syncer.discovered, syncer.inputs)
	}
}

func TestIncomingMetadataDiscoveryFailureDoesNotFetch(t *testing.T) {
	for _, err := range []error{context.Canceled, errors.New("forbidden")} {
		t.Run(err.Error(), func(t *testing.T) {
			root := newPostMergeBriefingRepo(t)
			shas := incomingCommitSHAs(root)
			pending := filepath.Join(root, ".cxt", "pending-local-sentinel")
			if err := os.WriteFile(pending, []byte("unpublished local work"), 0600); err != nil {
				t.Fatal(err)
			}
			syncer := &incomingMetadataSync{discoveryErr: err}
			handleIncomingContexts(context.Background(), &Container{Sync: syncer, PRMerges: &fakePRMergeResolver{}}, root)
			if !syncer.discovered || len(syncer.inputs) != 0 {
				t.Fatalf("discovery=%v fetches=%d", syncer.discovered, len(syncer.inputs))
			}
			if b, e := os.ReadFile(pending); e != nil || string(b) != "unpublished local work" {
				t.Fatal("failed discovery changed local work")
			}
			// A later Git operation must not erase the already persisted range.
			runGitForTest(t, root, "update-ref", "-d", "ORIG_HEAD")
			resolver := &fakePRMergeResolver{}
			replayPRDiscovery(context.Background(), resolver, &fakeMergedPRSync{}, root, "main", "https://github.com/acme/project.git", nil)
			if !reflect.DeepEqual(resolver.shas, shas) {
				t.Fatalf("saved PR range lost: %v want %v", resolver.shas, shas)
			}
		})
	}
}

func TestIncomingMetadataHydratesEveryCandidateBatchInOrder(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, count := range []int{0, 2, domain.MaxBranchPullRoots + 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := &incomingMetadataSync{allowPull: true}
			var shas []string
			var want []domain.ContentHash
			for i := 0; i < count; i++ {
				sha := fmt.Sprintf("%040x", i+1)
				id := briefingHash(sha)
				shas = append(shas, sha)
				want = append(want, id)
				s.catalog = append([]domain.Snapshot{{ID: id, Message: "candidate [git " + sha + "]"}}, s.catalog...)
			}
			out, err := fetchIncomingContexts(context.Background(), &Container{Sync: s}, t.TempDir(), "main", shas)
			if err != nil || out.Pulled != count {
				t.Fatalf("pulled %d/%d %v", out.Pulled, count, err)
			}
			var got []domain.ContentHash
			for _, in := range s.inputs {
				if in.Ref != "main" || !in.FetchOnly || !in.RequireBranchPlan || len(in.ObservationRoots) > domain.MaxBranchPullRoots {
					t.Fatalf("unscoped/oversized batch: %+v", in)
				}
				got = append(got, in.ObservationRoots...)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("lost/reordered candidate roots: %d/%d", len(got), len(want))
			}
			if len(s.inputs) != max(1, (count+domain.MaxBranchPullRoots-1)/domain.MaxBranchPullRoots) {
				t.Fatalf("batch count %d", len(s.inputs))
			}
		})
	}
}
