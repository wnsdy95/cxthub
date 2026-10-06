package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// This probe checks the application transaction-context boundary only. It is
// deliberately not a substitute for PostgreSQL rollback/concurrency testing.
type inheritedSelectionTxKey struct{}
type inheritedSelectionTxProbe struct {
	*store.FSStore
	reads                     map[domain.ContentHash]int
	corrupt                   domain.ContentHash
	histories, applies, locks int
}

func (s *inheritedSelectionTxProbe) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, inheritedSelectionTxKey{}, repo))
}
func (s *inheritedSelectionTxProbe) WithinReadSnapshot(context.Context, func(context.Context) error) error {
	return domain.ErrConflict
}
func (s *inheritedSelectionTxProbe) LockRepositoryAccess(ctx context.Context, _ string, _ string) error {
	if ctx.Value(inheritedSelectionTxKey{}) == nil {
		return domain.ErrConflict
	}
	s.locks++
	return nil
}
func (s *inheritedSelectionTxProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if ctx.Value(inheritedSelectionTxKey{}) != repo {
		return nil, domain.ErrConflict
	}
	s.histories++
	return s.FSStore.ListHistoryEvents(ctx, repo)
}
func (s *inheritedSelectionTxProbe) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	if ctx.Value(inheritedSelectionTxKey{}) != repo {
		return domain.Snapshot{}, domain.ErrConflict
	}
	return s.FSStore.GetSnapshot(ctx, repo, id)
}
func (s *inheritedSelectionTxProbe) GetMemory(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	if ctx.Value(inheritedSelectionTxKey{}) != repo {
		return domain.MemoryDigest{}, domain.ErrConflict
	}
	s.reads[id]++
	d, err := s.FSStore.GetMemory(ctx, repo, id)
	if id == s.corrupt {
		d.Summary += " corrupt"
	}
	return d, err
}
func (s *inheritedSelectionTxProbe) ApplyHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if ctx.Value(inheritedSelectionTxKey{}) != domain.ContentHash(e.RepoID) {
		return domain.ErrConflict
	}
	s.applies++
	return s.FSStore.ApplyHistoryEvent(ctx, e)
}

func TestInheritedHistoryMemorySelectionAcceptance(t *testing.T) {
	for _, mode := range []string{"valid chain", "prior context source", "explicit self successor", "missing ancestor", "foreign ancestor", "corrupt ancestor", "wrong explicit owner", "missing witness", "publish witness", "nonroot successor", "foreign successor owner", "other worktree", "other code", "other identity", "other alias", "viewer"} {
		t.Run(mode, func(t *testing.T) {
			svc, st := newFsckSvc(t)
			f := newHistoryMemorySelectionFixture(t, svc, st, hh(t.Name()))
			put := func(owner, previous domain.ContentHash, label string) domain.ContentHash {
				t.Helper()
				h, err := st.PutMemory(f.ctx, f.repo, domain.MemoryDigest{SnapshotID: owner, PreviousMemoryHash: previous, Summary: label})
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			ancestor := put(f.other, "", "ancestor")
			previous := ancestor
			if mode == "missing ancestor" {
				previous = hh("missing ancestor")
			}
			if mode == "foreign ancestor" {
				previous = put(f.before.Target, "", "foreign ancestor")
			}
			before, after := f.before, f.after
			before.MemoryHash = put(f.other, previous, "inherited descendant")
			before.MemorySource = f.other
			switch mode {
			case "prior context source":
				before.Source = f.other
			case "explicit self successor":
				after.MemorySource = after.Target
			case "wrong explicit owner":
				before.MemoryHash = put(before.Target, "", "actual owner differs from declared source")
			case "missing witness":
				after.MemorySelectionParent = strings.Repeat("9", 32)
			case "publish witness":
				ordinary := before
				ordinary.ID = strings.Repeat("3", 32)
				if err := svc.RecordHistory(f.ctx, ordinary); err != nil {
					t.Fatal(err)
				}
				before.Kind = "publish"
			case "nonroot successor":
				after.MemoryHash = put(after.Target, after.MemoryHash, "not root")
			case "foreign successor owner":
				after.MemoryHash = put(f.other, "", "wrong successor owner")
			case "other worktree":
				after.WorktreeID = strings.Repeat("9", 32)
			case "other code":
				after.GitAfter = strings.Repeat("9", 40)
			case "other identity":
				after.BranchID = "other"
			case "other alias":
				after.LocalBranch = "other"
			}
			if err := svc.RecordHistory(f.ctx, before); err != nil {
				t.Fatal("ordinary predecessor setup", err)
			}
			eventsBefore, err := st.ListHistoryEvents(f.ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			refsBefore, err := st.ListRefs(f.ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			probe := &inheritedSelectionTxProbe{FSStore: st, reads: map[domain.ContentHash]int{}}
			if mode == "corrupt ancestor" {
				probe.corrupt = ancestor
			}
			writer := NewService(probe, probe, nil, nil, probe)
			if mode == "viewer" {
				if err := st.AddMember(f.ctx, domain.Membership{RepositoryID: f.repository, UserID: f.member, Role: domain.RoleViewer}); err != nil {
					t.Fatal(err)
				}
			}
			err = writer.RecordHistory(f.ctx, after)
			success := mode == "valid chain" || mode == "prior context source" || mode == "explicit self successor"
			if success && err != nil {
				t.Fatalf("valid inherited dependency rejected: %v", err)
			}
			if !success && err == nil {
				t.Fatal("invalid predecessor/root accepted")
			}
			if mode == "viewer" && !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("authorization=%v", err)
			}
			eventsAfter, listErr := st.ListHistoryEvents(f.ctx, f.repo)
			if listErr != nil {
				t.Fatal(listErr)
			}
			refsAfter, listErr := st.ListRefs(f.ctx, f.repo)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if !success {
				if probe.applies != 0 || !reflect.DeepEqual(eventsBefore, eventsAfter) || !reflect.DeepEqual(refsBefore, refsAfter) {
					t.Fatal("rejection changed history/refs")
				}
				return
			}
			if probe.histories == 0 || probe.locks == 0 || probe.applies != 1 || probe.reads[ancestor] == 0 || probe.reads[before.MemoryHash] == 0 || probe.reads[after.MemoryHash] == 0 {
				t.Fatal("accepted witness, full ancestry, root and apply did not share bound transaction")
			}
			if !reflect.DeepEqual(historyMemorySelectionBranches(refsBefore), historyMemorySelectionBranches(refsAfter)) {
				t.Fatal("selection moved branch")
			}
			for _, want := range []domain.HistoryEvent{before, after} {
				found := false
				for _, e := range eventsAfter {
					if reflect.DeepEqual(e, want) {
						found = true
					}
				}
				if !found {
					t.Fatal("raw immutable proof lost")
				}
			}
			// Advance the branch independently, then make predecessor bytes
			// unreadable through this request adapter. Exact receipt replay
			// must neither read the chain nor rewind/reapply the old operation.
			ref, e := st.GetRef(f.ctx, f.repo, domain.RefBranch, "main")
			if e != nil {
				t.Fatal(e)
			}
			old := ref.Target
			ref.Target = f.other
			if e = st.CompareAndSwapRef(f.ctx, f.repo, ref, old); e != nil {
				t.Fatal(e)
			}
			probe.corrupt = ancestor
			reads := probe.reads[ancestor]
			if e = writer.RecordHistory(f.ctx, after); e != nil {
				t.Fatal("exact replay", e)
			}
			if probe.applies != 1 || probe.reads[ancestor] != reads {
				t.Fatal("receipt replay reapplied/revalidated")
			}
			changed := after
			changed.MemorySelectionParent = strings.Repeat("8", 32)
			if e = writer.RecordHistory(f.ctx, changed); !errors.Is(e, domain.ErrRefConflict) {
				t.Fatalf("ID collision=%v", e)
			}
			got, e := st.GetRef(f.ctx, f.repo, domain.RefBranch, "main")
			if e != nil || got != ref {
				t.Fatal("retry rewound branch", e)
			}
		})
	}
}
