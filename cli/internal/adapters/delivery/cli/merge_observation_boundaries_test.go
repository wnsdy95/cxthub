package cli

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type mergeObservationRefOnly struct {
	inbound.SyncRepo
	ref       domain.Ref
	refCalls  int
	appends   []domain.ContentHash
	appendErr error
}

func (s *mergeObservationRefOnly) ResolveRemoteBranch(context.Context, inbound.SyncInput, string) (domain.Ref, error) {
	s.refCalls++
	return s.ref, nil
}
func (s *mergeObservationRefOnly) AppendBranch(_ context.Context, _ inbound.SyncInput, _ string, target domain.ContentHash) error {
	s.appends = append(s.appends, target)
	return s.appendErr
}

type mergeObservationBoundarySync struct {
	*mergeObservationRefOnly
	observed inbound.RemoteBranchObservation
	err      error
	calls    int
}

func (s *mergeObservationBoundarySync) ResolveRemoteBranchObservation(context.Context, inbound.SyncInput, string) (inbound.RemoteBranchObservation, error) {
	s.calls++
	return s.observed, s.err
}

func TestMergeObservationBoundaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, mode := range []string{"missing_observer", "observer_error", "empty_remote_target", "failed_append", "behind_append", "wrapped_behind_append", "misleading_text_append", "wrong_status_append", "new_observed_tip"} {
		t.Run(mode, func(t *testing.T) {
			r := domain.HashContent([]byte("synthetic old remote tip"))
			a := domain.HashContent([]byte("synthetic merge candidate"))
			y := domain.HashContent([]byte("synthetic new remote tip"))
			local := []domain.Snapshot{{ID: a, Message: "candidate [git aaaa]"}, {ID: r}}
			base := &mergeObservationRefOnly{ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: r}}
			observer := &mergeObservationBoundarySync{mergeObservationRefOnly: base, observed: inbound.RemoteBranchObservation{Ref: base.ref, Snapshots: local}}
			var syncer inbound.SyncRepo = observer
			var wantAppends []domain.ContentHash
			wantReflected, wantObservations := false, 1
			switch mode {
			case "missing_observer":
				syncer, wantObservations = base, 0
			case "observer_error":
				observer.err = fmt.Errorf("synthetic observation failure")
			case "empty_remote_target":
				observer.observed.Ref.Target = ""
			case "failed_append":
				base.appendErr = fmt.Errorf("synthetic append failure")
				wantAppends = []domain.ContentHash{a}
			case "behind_append":
				base.appendErr = &backendclient.HTTPError{Status: 409, Code: "non_fast_forward"}
				wantAppends, wantReflected = []domain.ContentHash{a}, true
			case "wrapped_behind_append":
				base.appendErr = fmt.Errorf("append rejected: %w", &backendclient.HTTPError{Status: 409, Code: "non_fast_forward"})
				wantAppends, wantReflected = []domain.ContentHash{a}, true
			case "misleading_text_append":
				base.appendErr = fmt.Errorf("cannot reach branch fix/non_fast_forward")
				wantAppends = []domain.ContentHash{a}
			case "wrong_status_append":
				base.appendErr = &backendclient.HTTPError{Status: 403, Code: "non_fast_forward"}
				wantAppends = []domain.ContentHash{a}
			case "new_observed_tip":
				// Local list has no Y. Only this fetch proves new remote Y→A.
				observer.observed.Ref.Target = y
				observer.observed.Snapshots = []domain.Snapshot{{ID: a}, {ID: y, Parents: []domain.ContentHash{a}}}
				wantReflected = true
			}
			got := appendMergedContexts(context.Background(), &Container{List: fixedBriefingList{out: inbound.ListOutput{Snapshots: local}}, Sync: syncer}, t.TempDir(), "main", []string{"aaaa1111"})
			if got != wantReflected || !reflect.DeepEqual(base.appends, wantAppends) {
				t.Fatalf("reflected=%t want=%t; append count=%d want=%d", got, wantReflected, len(base.appends), len(wantAppends))
			}
			if observer.calls != wantObservations || base.refCalls != 0 {
				t.Fatalf("observer calls=%d want=%d; forbidden ref-only calls=%d", observer.calls, wantObservations, base.refCalls)
			}
		})
	}
}
