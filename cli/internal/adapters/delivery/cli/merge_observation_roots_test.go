package cli

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type mergeRootObserver struct {
	*mergeObservationBoundarySync
	roots []domain.ContentHash
}

func (s *mergeRootObserver) ResolveRemoteBranchObservation(ctx context.Context, in inbound.SyncInput, branch string) (inbound.RemoteBranchObservation, error) {
	s.roots = append([]domain.ContentHash(nil), in.ObservationRoots...)
	return s.mergeObservationBoundarySync.ResolveRemoteBranchObservation(ctx, in, branch)
}

func TestMergeObservationCandidateRoots(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, count := range []int{2, domain.MaxBranchPullRoots + 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			remote := domain.HashContent([]byte("separate remote head"))
			var shas []string
			var ids []domain.ContentHash
			var local []domain.Snapshot
			for i := 1; i <= count; i++ {
				sha := fmt.Sprintf("%040x", i)
				id := domain.HashContent([]byte(sha))
				shas = append(shas, sha)
				ids = append(ids, id)
				local = append([]domain.Snapshot{{ID: id, Message: "candidate [git " + sha + "]"}}, local...)
			}
			base := &mergeObservationRefOnly{ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: remote}}
			observer := &mergeRootObserver{mergeObservationBoundarySync: &mergeObservationBoundarySync{
				mergeObservationRefOnly: base,
				observed:                inbound.RemoteBranchObservation{Ref: base.ref, Snapshots: []domain.Snapshot{{ID: remote}}},
			}}
			wantAppends := ids
			if count == 2 {
				// The candidate is outside main's closure, but the optional-root
				// observation proves its ancestry and avoids a redundant append.
				observer.observed.Snapshots = append(observer.observed.Snapshots, domain.Snapshot{ID: ids[1], Parents: []domain.ContentHash{ids[0]}}, domain.Snapshot{ID: ids[0]})
				wantAppends = ids[1:]
			}
			got := appendMergedContexts(context.Background(), &Container{List: fixedBriefingList{out: inbound.ListOutput{Snapshots: local}}, Sync: observer}, t.TempDir(), "main", shas, false)
			wantRoots := ids
			if len(wantRoots) > domain.MaxBranchPullRoots {
				wantRoots = wantRoots[len(wantRoots)-domain.MaxBranchPullRoots:]
			}
			if !got || !reflect.DeepEqual(observer.roots, wantRoots) || !reflect.DeepEqual(base.appends, wantAppends) {
				t.Fatalf("reflected=%v roots=%d/%d appends=%d/%d; roots or append order differ", got, len(observer.roots), len(wantRoots), len(base.appends), len(wantAppends))
			}
			if observer.calls != 1 || base.refCalls != 0 {
				t.Fatalf("unexpected observation calls %d, ref-only calls %d", observer.calls, base.refCalls)
			}
		})
	}
}
