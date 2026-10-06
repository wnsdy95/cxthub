package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Use the real FileStore/outbox fixture. The callbacks model an
// independent writer between successful ref publication and status cleanup.
type publicationInterleavingRemote struct {
	*strictPublicationRemote
	afterRefs          func()
	unsyncTarget       domain.ContentHash
	loseSecondGraftAck bool
}

func (r *publicationInterleavingRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if err := r.strictPublicationRemote.Push(ctx, repo, snaps, docs, refs, force, appendMode); err != nil {
		return err
	}
	if len(refs) > 0 && r.afterRefs != nil {
		r.afterRefs()
	}
	return nil
}

func (r *publicationInterleavingRemote) DeleteUnsyncRemote(ctx context.Context, repo, name string) error {
	// Actual HTTP/service/store API deletes by (repo,user,name), with no
	// expected target or identity. Do not give the stub a stronger CAS.
	r.unsyncTarget = ""
	return r.strictPublicationRemote.DeleteUnsyncRemote(ctx, repo, name)
}

func (r *publicationInterleavingRemote) GraftSnapshotParents(ctx context.Context, repo string, id domain.ContentHash, parents []domain.ContentHash, seq uint64) error {
	if err := r.strictPublicationRemote.GraftSnapshotParents(ctx, repo, id, parents, seq); err != nil {
		return err
	}
	if r.loseSecondGraftAck && len(r.grafts) == 2 {
		return errors.New("second graft accepted; acknowledgement lost")
	}
	return nil
}

func TestPublicationPreservesUncoveredUnsync(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "newer-unpublished-target"
		if reuse {
			name = "name-reused-by-foreign-identity"
		}
		t.Run(name, func(t *testing.T) {
			f := newStrictPublication(t)
			r := &publicationInterleavingRemote{strictPublicationRemote: f.r}
			r.afterRefs = func() {
				r.unsyncTarget = f.x
				if reuse {
					r.setRef(domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", BranchID: "Y", Target: f.a})
				}
			}
			f.svc.remote = r
			if _, err := f.svc.Push(f.ctx, f.input()); err != nil {
				t.Fatal(err)
			}
			if r.unsyncTarget != f.x {
				t.Fatalf("scoped R/B push cleared uncovered status at X: reused=%v deletes=%v", reuse, r.unsync)
			}
		})
	}
}

func TestPublicationTwoEventFIFO(t *testing.T) {
	for _, scenario := range []string{"replace-second", "remove-second", "lose-second-ack"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStrictPublication(t)
			c := publicationSnapshot(t, f.st, f.repo, "C", nil, nil)
			f.graft(f.b, f.a)
			f.graft(f.b, c)
			initial, err := readGraftQueue(f.root, "")
			if err != nil || len(initial.Events) != 2 {
				t.Fatalf("fixture queue: %+v %v", initial, err)
			}
			r := &publicationInterleavingRemote{strictPublicationRemote: f.r, loseSecondGraftAck: scenario == "lose-second-ack"}
			f.svc.remote = r
			if scenario != "lose-second-ack" {
				r.onGraft = func() {
					changed := append([]domain.GraftQueueEvent(nil), initial.Events...)
					if scenario == "replace-second" {
						changed[1].ExpectedSeq += 4
					} else {
						changed = changed[:1]
					}
					f.queue(changed)
				}
			}
			_, err = f.svc.Push(f.ctx, f.input())
			if err == nil {
				t.Fatal("stale/lost acknowledgement reported success")
			}
			f.noPublication()
			q, qerr := readGraftQueue(f.root, "")
			if qerr != nil {
				t.Fatal(qerr)
			}
			if !sameGraftQueueEvent(r.grafts[0], initial.Events[0]) {
				t.Fatal("first payload changed")
			}
			switch scenario {
			case "replace-second":
				if !errors.Is(err, domain.ErrSyncConflict) || len(r.grafts) != 1 || len(q.Events) != 1 || q.Events[0].ExpectedSeq != initial.Events[1].ExpectedSeq+4 {
					t.Fatalf("replacement sent/acknowledged: %v %+v %+v", err, r.grafts, q.Events)
				}
			case "remove-second":
				if !errors.Is(err, domain.ErrSyncConflict) || len(r.grafts) != 1 || len(q.Events) != 0 {
					t.Fatalf("disappearance became acknowledgement: %v %+v %+v", err, r.grafts, q.Events)
				}
			case "lose-second-ack":
				if len(r.grafts) != 2 || len(q.Events) != 1 || !sameGraftQueueEvent(q.Events[0], initial.Events[1]) {
					t.Fatalf("unacknowledged second removed: %+v %+v", r.grafts, q.Events)
				}
			}
		})
	}
}
