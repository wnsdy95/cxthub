package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPullReadKey struct{}
type branchPullStore struct {
	*store.FSStore
	t           *testing.T
	events      []domain.HistoryEvent
	reads       int
	memoryCalls int
	readErr     error
}

func (s *branchPullStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	s.t.Fatal("plan attempted write transaction")
	return nil
}
func (s *branchPullStore) WithinReadSnapshot(ctx context.Context, f func(context.Context) error) error {
	if ctx.Value(branchPullReadKey{}) == true {
		return f(ctx)
	}
	s.reads++
	if err := f(context.WithValue(ctx, branchPullReadKey{}, true)); err != nil {
		return err
	}
	return s.readErr
}
func (s *branchPullStore) check(ctx context.Context) {
	s.t.Helper()
	if ctx.Value(branchPullReadKey{}) != true {
		s.t.Fatal("read escaped repositoryRead")
	}
}
func (s *branchPullStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	s.check(ctx)
	return s.FSStore.GetRepo(ctx, id)
}
func (s *branchPullStore) ListRefs(ctx context.Context, id domain.ContentHash) ([]domain.Ref, error) {
	s.check(ctx)
	return s.FSStore.ListRefs(ctx, id)
}
func (s *branchPullStore) ListSnapshots(ctx context.Context, id domain.ContentHash, b string) ([]domain.Snapshot, error) {
	s.check(ctx)
	return s.FSStore.ListSnapshots(ctx, id, b)
}
func (s *branchPullStore) ListHistoryEvents(ctx context.Context, id domain.ContentHash) ([]domain.HistoryEvent, error) {
	s.check(ctx)
	return s.events, nil
}
func (s *branchPullStore) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	s.memoryCalls++
	panic("branch plan opened memory body")
}
func (s *branchPullStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.t.Fatal("plan opened transcript")
	return domain.SessionDoc{}, nil
}
func (s *branchPullStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	s.t.Fatal("plan opened doc manifest")
	return domain.DocChunkManifest{}, nil
}
func (s *branchPullStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	s.t.Fatal("plan opened doc chunk")
	return nil, nil
}
func branchPullService(t *testing.T) (*Service, *branchPullStore, domain.ContentHash, domain.ContentHash) {
	t.Helper()
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	repo, tip := hh("branch-plan repo"), hh("tip")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: tip, DocHash: tip}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: tip}, ""); err != nil {
		t.Fatal(err)
	}
	spy := &branchPullStore{FSStore: st, t: t}
	return NewService(spy, spy, nil, nil, nil), spy, repo, tip
}
func putPlanMemory(t *testing.T, st *store.FSStore, repo, owner, previous domain.ContentHash, label string) domain.ContentHash {
	t.Helper()
	h, err := st.PutMemory(context.Background(), repo, domain.MemoryDigest{SnapshotID: owner, PreviousMemoryHash: previous, Summary: label})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The plan carries metadata roots, including an unavailable historical body.
// Cold and unchanged warm requests must both avoid all immutable-memory reads.
func TestBranchPullPlanColdWarmMetadataOnly(t *testing.T) {
	svc, st, repo, tip := branchPullService(t)
	ctx := context.Background()
	owner := hh("historical inherited owner")
	if err := st.FSStore.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: owner, DocHash: owner}); err != nil {
		t.Fatal(err)
	}
	current := putPlanMemory(t, st.FSStore, repo, owner, "", "current attachment")
	if err := st.FSStore.CompareAndSwapSnapshotMemory(ctx, repo, owner, "", current); err != nil {
		t.Fatal(err)
	}
	snap, err := st.FSStore.GetSnapshot(ctx, repo, owner)
	if err != nil {
		t.Fatal(err)
	}
	token, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	pin := hh("historical root not materialized by planner")
	event := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: domain.LegacyContextBranchID(string(repo), "main"), Branch: "main", Kind: "position", GitAfter: strings.Repeat("1", 40), Target: tip, MemoryPinned: true, MemorySource: owner, MemoryHash: pin, CreatedAt: time.Unix(1, 0)}
	empty := event
	empty.ID, empty.MemoryHash, empty.MemorySource, empty.CreatedAt = strings.Repeat("b", 32), "", "", time.Unix(2, 0)
	st.events = []domain.HistoryEvent{event, empty}
	before, err := st.FSStore.GetManifest(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var cold domain.BranchPullPlan
	for i, name := range []string{"cold", "warm"} {
		t.Run(name, func(t *testing.T) {
			plan, err := svc.PullBranchPlan(ctx, repo, domain.BranchPullRequest{Version: 1, Branch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			if st.memoryCalls != 0 || st.reads != i+1 {
				t.Fatalf("memory calls=%d reads=%d", st.memoryCalls, st.reads)
			}
			if len(plan.SnapshotIndex) != 2 || plan.SnapshotStates[owner] != token || !reflect.DeepEqual(plan.History, st.events) {
				t.Fatalf("frozen roots changed: %+v", plan)
			}
			if i == 0 {
				cold = plan
			} else if !reflect.DeepEqual(cold, plan) {
				t.Fatal("unchanged warm plan changed")
			}
		})
	}
	// Attachment hashes are part of the token, while pinned empty stays empty.
	changed := snap
	changed.MemoryHash = hh("later attachment")
	changedToken, err := domain.SnapshotStateHash(changed)
	if err != nil || changedToken == token || snap.MemoryHash != current {
		t.Fatal("attachment not frozen by state token")
	}
	after, err := st.FSStore.GetManifest(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	before.UpdatedAt = after.UpdatedAt
	if !reflect.DeepEqual(before, after) {
		t.Fatal("plan mutated repository")
	}
}

func TestBranchPullPlanDiscardsReadCompletionFailure(t *testing.T) {
	svc, st, repo, _ := branchPullService(t)
	st.readErr = errors.New("injected read transaction completion failure")
	p, err := svc.PullBranchPlan(context.Background(), repo, domain.BranchPullRequest{Version: 1, Branch: "main"})
	if !errors.Is(err, st.readErr) || !reflect.DeepEqual(p, domain.BranchPullPlan{}) || st.memoryCalls != 0 {
		t.Fatalf("transaction completion returned partial proof: %+v %v memory calls=%d", p, err, st.memoryCalls)
	}
}
