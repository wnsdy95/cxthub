package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextWriteReadSpy struct {
	*store.FSStore
	protocol                      int
	repoReads, historyReads, refs int
	repoErr, historyErr, refErr   error
}

func (s *contextWriteReadSpy) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	s.repoReads++
	if s.repoErr != nil {
		return domain.Repo{}, s.repoErr
	}
	r, err := s.FSStore.GetRepo(ctx, repo)
	r.ContextProtocol = s.protocol
	return r, err
}

func (s *contextWriteReadSpy) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	s.historyReads++
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	return s.FSStore.ListHistoryEvents(ctx, repo)
}

func (s *contextWriteReadSpy) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	s.refs++
	if s.refErr != nil {
		return domain.Ref{}, s.refErr
	}
	return s.FSStore.GetRef(ctx, repo, kind, name)
}

func contextWriteService(t *testing.T) (*Service, *contextWriteReadSpy, domain.Ref) {
	t.Helper()
	ctx := systemTestContext()
	fs := store.NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := fs.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: domain.HashContent([]byte("target"))}
	if err := fs.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: ref.Target, DocHash: ref.Target}); err != nil {
		t.Fatal(err)
	}
	if err := fs.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	ref.BranchID = domain.LegacyContextBranchID(string(repo), ref.Name)
	spy := &contextWriteReadSpy{FSStore: fs, protocol: 1}
	return NewService(spy, fs, nil, nil, nil), spy, ref
}

func TestCheckContextWriteReadCounts(t *testing.T) {
	for _, protocol := range []int{0, 1, 2} {
		for _, kind := range []domain.RefKind{domain.RefBranch, domain.RefHead, domain.RefTag, domain.RefSession} {
			t.Run(fmt.Sprintf("%d/%s", protocol, kind), func(t *testing.T) {
				svc, spy, ref := contextWriteService(t)
				spy.protocol, ref.Kind = protocol, kind
				err := svc.checkContextWrite(systemTestContext(), ref.RepoID, ref)
				want := 0
				if protocol == 1 && kind == domain.RefBranch {
					want = 1
				}
				t.Logf("repo=%d history=%d current-ref=%d", spy.repoReads, spy.historyReads, spy.refs)
				// The public history read adds its own pinned compatibility check.
				if spy.repoReads != 1+want || spy.historyReads != want || spy.refs != want {
					t.Fatalf("want repo=%d history=%d current-ref=%d", 1+want, want, want)
				}
				if protocol == 2 {
					if !errors.Is(err, domain.ErrConflict) {
						t.Fatalf("unsupported version accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCheckContextWriteReadFailures(t *testing.T) {
	svc, spy, branch := contextWriteService(t)
	ctx := systemTestContext()
	stop := errors.New("injected read failure")
	spy.historyErr, spy.refErr = stop, stop
	for _, kind := range []domain.RefKind{domain.RefHead, domain.RefTag, domain.RefSession} {
		ref := branch
		ref.Kind = kind
		if err := svc.checkContextWrite(ctx, ref.RepoID, ref); err != nil {
			t.Fatalf("nonbranch read unused state: %v", err)
		}
	}
	missingID := branch
	missingID.BranchID = ""
	if err := svc.checkContextWrite(ctx, branch.RepoID, missingID); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("identity preflight", err)
	}
	if spy.historyReads != 0 || spy.refs != 0 {
		t.Fatalf("unnecessary history=%d current-ref=%d", spy.historyReads, spy.refs)
	}
	if err := svc.checkContextWrite(ctx, branch.RepoID, branch); !errors.Is(err, stop) {
		t.Fatal("branch history error lost", err)
	}
	spy.historyErr = nil
	if err := svc.checkContextWrite(ctx, branch.RepoID, branch); !errors.Is(err, stop) {
		t.Fatal("branch current-ref error lost", err)
	}
	spy.refErr = nil
	stale := branch
	stale.BranchID = "stale"
	if err := svc.checkContextWrite(ctx, branch.RepoID, stale); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("branch identity conflict lost", err)
	}
	branch.Kind = domain.RefTag
	spy.repoErr = stop
	if err := svc.checkContextWrite(ctx, branch.RepoID, branch); !errors.Is(err, stop) {
		t.Fatal("repository error lost", err)
	}
	// Preserve the existing compatibility behavior for absent repository metadata.
	spy.repoErr = domain.ErrNotFound
	if err := svc.checkContextWrite(ctx, branch.RepoID, branch); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRefContextPreflightPreservesChecks(t *testing.T) {
	svc, spy, branch := contextWriteService(t)
	ctx := systemTestContext()
	spy.historyErr = errors.New("nonbranch must not read context history")
	tag := domain.Ref{RepoID: branch.RepoID, Kind: domain.RefTag, Name: "release", Target: branch.Target}
	in := inbound.UpdateRefInput{RepoID: tag.RepoID, Ref: tag}
	if _, err := svc.UpdateRef(context.Background(), in); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("authorization lost", err)
	}
	if _, err := svc.UpdateRef(ctx, in); err != nil {
		t.Fatal("nonbranch write", err)
	}
	if spy.historyReads != 0 || spy.refs != 1 {
		t.Fatalf("write reads: history=%d current-ref=%d; want 0/1 (CAS observation retained)", spy.historyReads, spy.refs)
	}
	other := domain.HashContent([]byte("other target"))
	if err := spy.PutSnapshot(ctx, domain.Snapshot{RepoID: tag.RepoID, ID: other, DocHash: other}); err != nil {
		t.Fatal(err)
	}
	in.Ref.Target, in.ExpectedTarget, in.Force = other, other, true
	// A different third target makes the stale expectation unambiguously stale.
	in.ExpectedTarget = domain.HashContent([]byte("stale observation"))
	if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("CAS conflict lost", err)
	}
	in.Ref.Target = domain.HashContent([]byte("missing target"))
	if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("target check lost", err)
	}
	in.Ref = domain.Ref{Kind: domain.RefHead, Name: domain.HeadRefName, Symbolic: "missing"}
	if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("symbolic root check lost", err)
	}
	in.Ref = tag
	in.Ref.Name = "../invalid"
	if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("ref validation lost", err)
	}
	in.Ref = tag
	in.Ref.Kind = domain.RefSession
	in.ExpectedTarget, in.Force = "", false
	if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("internal session restriction lost", err)
	}
}
