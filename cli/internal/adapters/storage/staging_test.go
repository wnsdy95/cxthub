package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type frozenFixture struct {
	store    *FileStore
	repo     string
	index    domain.StagingIndex
	position domain.WorkingPosition
	op       domain.StagingCommit
}

func newFrozenFixture(t *testing.T) frozenFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", sha)
	repo := string(domain.HashContent([]byte(root)))
	ids := []domain.ContentHash{}
	for _, label := range []string{"base", "frozen"} {
		h, err := s.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SourceProvider: domain.ProviderClaude, SessionOriginID: label}, Events: []domain.Event{}}})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: h, DocHash: h, RepoID: repo, Branch: "main", Provider: domain.ProviderClaude, SessionID: label}
		if len(ids) > 0 {
			snap.Parents = []domain.ContentHash{ids[0]}
		}
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h)
	}
	branchID := domain.LegacyContextBranchID(repo, "main")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: branchID, Target: ids[0]}
	if _, err := s.CreateBranchRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", BranchID: branchID, GitCommit: sha, Snapshot: ids[0], SharedTarget: ids[0]}); err != nil {
		t.Fatal(err)
	}
	i, p, err := s.ReadStaging(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	entry := domain.StagedSession{Provider: domain.ProviderClaude, SessionID: "frozen", SourceID: domain.HashContent([]byte("source")), Generation: ids[1], DocHash: ids[1], CodeCommit: sha, Branch: "main", BranchID: branchID, Base: ids[0], CapturedAt: time.Now().UTC()}
	entry.Key = domain.StagedSessionKey(entry.Provider, entry.SessionID, entry.SourceID, entry.Generation)
	next := i
	next.Sequence++
	next.Entries = []domain.StagedSession{entry}
	next = next.WithRevision()
	if err := s.CompareAndSwapStaging(ctx, i.Revision, next, p); err != nil {
		t.Fatal(err)
	}
	nextPosition := p
	nextPosition.Snapshot = ids[1]
	nextPosition.SharedTarget = ids[1]
	nextPosition.MemoryPinned = true
	nextPosition.Selection = &domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: repo, WorktreeID: s.worktreeID, Branch: "main", BranchID: branchID, Kind: "publish", Source: ids[1], Target: ids[1], MemoryPinned: true, GitAfter: sha, CreatedAt: time.Now().UTC()}
	nextRef := ref
	nextRef.Target = ids[1]
	op := domain.StagingCommit{Version: domain.StagingVersion, ID: nextPosition.Selection.ID, Index: next, ExpectedPosition: p, ExpectedRef: ref, Position: nextPosition, Ref: nextRef, CreatedAt: time.Now().UTC()}
	return frozenFixture{s, repo, next, p, op}
}

func TestStagingIndexWorktreeIsolationAndConcurrentCAS(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	peer := NewWorktreeFileStore(f.store.repoRoot, filepath.Join(f.store.repoRoot, ".git", "worktrees", "peer"), "main", f.store.gitCommit)
	i, _, err := peer.ReadStaging(ctx, f.repo)
	if err != nil || len(i.Entries) != 0 || i.WorktreeID == f.index.WorktreeID {
		t.Fatal(i, err)
	}
	first, second := f.index, f.index
	first.Sequence++
	second.Sequence++
	first.Entries = []domain.StagedSession{}
	second.Entries = append([]domain.StagedSession{}, second.Entries...)
	second.Entries[0].CapturedBytes = 10
	first = first.WithRevision()
	second = second.WithRevision()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []domain.StagingIndex{first, second} {
		wg.Add(1)
		go func(i domain.StagingIndex) {
			defer wg.Done()
			<-start
			results <- f.store.CompareAndSwapStaging(ctx, f.index.Revision, i, f.position)
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, domain.ErrSyncConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	i, _, err = peer.ReadStaging(ctx, f.repo)
	if err != nil || len(i.Entries) != 0 {
		t.Fatal("other worktree changed", i, err)
	}
}

func TestStagingCommitRecoveryAfterEveryPublicationBoundary(t *testing.T) {
	for _, point := range []string{"prepared", "journal", "position", "history", "consumed"} {
		t.Run(point, func(t *testing.T) {
			f := newFrozenFixture(t)
			s := f.store
			ctx := context.Background()
			if err := s.writeStagingOperation(f.op); err != nil {
				t.Fatal(err)
			}
			if point == "journal" {
				raw, _ := json.Marshal(workingCommit{Ref: f.op.Ref, Expected: f.op.ExpectedRef.Target, Position: f.op.Position})
				if err := writeAtomic(s.workingCommitPath(), raw); err != nil {
					t.Fatal(err)
				}
			}
			if point == "position" || point == "history" || point == "consumed" {
				if err := s.putRefRaw(f.op.Ref); err != nil {
					t.Fatal(err)
				}
				if err := s.writePosition(f.op.Position); err != nil {
					t.Fatal(err)
				}
			}
			if point == "history" || point == "consumed" {
				if err := s.putHistoryEvent(*f.op.Position.Selection); err != nil {
					t.Fatal(err)
				}
			}
			if point == "consumed" {
				if err := s.writeStaging(domain.ConsumeStagedEntries(f.index, f.index.Entries)); err != nil {
					t.Fatal(err)
				}
			}
			restarted := NewWorktreeFileStore(s.repoRoot, filepath.Join(s.repoRoot, ".git"), "main", s.gitCommit)
			op, err := restarted.ResumeStagingCommit(ctx, f.repo, f.op.ID)
			if err != nil || !op.LocalFinalized {
				t.Fatal(op, err)
			}
			index, p, err := restarted.ReadStaging(ctx, f.repo)
			if err != nil || len(index.Entries) != 0 || p.Snapshot != f.op.Ref.Target {
				t.Fatal(index, p, err)
			}
			prior := index.Revision
			op, err = restarted.ResumeStagingCommit(ctx, f.repo, f.op.ID)
			if err != nil || !op.LocalFinalized {
				t.Fatal(op, err)
			}
			index, _, err = restarted.ReadStaging(ctx, f.repo)
			if err != nil || index.Revision != prior {
				t.Fatal("replay consumed twice", index, err)
			}
		})
	}
}

func TestStagingReceiptRecoveryPreservesConcurrentReaddAndLaterPosition(t *testing.T) {
	f := newFrozenFixture(t)
	s := f.store
	ctx := context.Background()
	if err := s.writeStagingOperation(f.op); err != nil {
		t.Fatal(err)
	}
	if err := s.putRefRaw(f.op.Ref); err != nil {
		t.Fatal(err)
	}
	if err := s.writePosition(f.op.Position); err != nil {
		t.Fatal(err)
	}
	// Model an older/newer binary writing another valid index while the original
	// operation's acknowledgement was interrupted; only exact entries are consumed.
	winner := f.index
	winner.Sequence++
	winner.Entries = append([]domain.StagedSession{}, winner.Entries...)
	winner.Entries[0].CapturedAt = winner.Entries[0].CapturedAt.Add(time.Second)
	winner = winner.WithRevision()
	if err := s.writeStaging(winner); err != nil {
		t.Fatal(err)
	}
	later := f.op.Position
	later.Selection = nil
	later.MemoryPinned = false
	if err := s.PutWorkingPosition(ctx, later); err != nil {
		t.Fatal(err)
	}
	later, err := s.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeStagingCommit(ctx, f.repo, f.op.ID); err != nil {
		t.Fatal(err)
	}
	actual, p, err := s.ReadStaging(ctx, f.repo)
	if err != nil || actual.Revision != winner.Revision || !reflect.DeepEqual(p, later) {
		t.Fatal("recovery replaced later work", actual, p, err)
	}
}

func TestStagingReadIsPureAndCorruptionFailsClosed(t *testing.T) {
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	repo := string(domain.HashContent([]byte(root)))
	if _, _, err := s.ReadStaging(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("read created local state", err)
	}
	f := newFrozenFixture(t)
	future := f.index
	future.Version = domain.RootStagingVersion + 1
	future = future.WithRevision()
	raw, _ := json.Marshal(future)
	if err := writeAtomic(f.store.stagingPath(), raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.ReadStaging(context.Background(), f.repo); !errors.Is(err, domain.ErrStagingVersion) {
		t.Fatal(err)
	}
	if _, err := f.store.HasStagingPin(context.Background(), f.op.Ref.Target); !errors.Is(err, domain.ErrStagingVersion) {
		t.Fatal("GC treated unsupported index as absent", err)
	}
}

func TestStagingPreparedCommitDoesNotOverrideCompetingPosition(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	if err := f.store.writeStagingOperation(f.op); err != nil {
		t.Fatal(err)
	}
	winner := f.position
	selection := *winner.Selection
	selection.ID = strings.Repeat("c", 32)
	winner.Selection = &selection
	if err := f.store.PutWorkingPosition(ctx, winner); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResumeStagingCommit(ctx, f.repo, f.op.ID); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal(err)
	}
	index, _, err := f.store.ReadStaging(ctx, f.repo)
	if err != nil || index.Revision != f.index.Revision {
		t.Fatal("failed operation consumed index", index, err)
	}
}

func TestStagingLegacySelectorIsNotAnIndexFormat(t *testing.T) {
	f := newFrozenFixture(t)
	if err := writeAtomic(f.store.stagingPath(), []byte(`{"staged":["claude","codex"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.ReadStaging(context.Background(), f.repo); !errors.Is(err, domain.ErrStagingVersion) {
		t.Fatal("legacy provider selection accepted as frozen index", err)
	}
	if _, err := f.store.HasStagingPin(context.Background(), f.op.Ref.Target); !errors.Is(err, domain.ErrStagingVersion) {
		t.Fatal("ambiguous index format did not protect GC", err)
	}
}

func TestStagingStashConflictPreservesSavedIndexAndPins(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	s := f.store
	stash := domain.StagingStash{Version: domain.StagingVersion, ID: strings.Repeat("d", 32), Index: f.index, Position: f.position, CreatedAt: time.Now().UTC()}
	if _, err := s.StashIndex(ctx, stash); err != nil {
		t.Fatal(err)
	}
	empty, p, err := s.ReadStaging(ctx, f.repo)
	if err != nil || len(empty.Entries) != 0 {
		t.Fatal(empty, err)
	}
	if pinned, err := s.HasStagingPin(ctx, f.op.Ref.Target); err != nil || !pinned {
		t.Fatal("stash source not protected", pinned, err)
	}
	winner := empty
	winner.Sequence++
	winner.Entries = append([]domain.StagedSession{}, f.index.Entries...)
	winner.Entries[0].CapturedBytes++
	winner = winner.WithRevision()
	if err := s.CompareAndSwapStaging(ctx, empty.Revision, winner, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PopIndex(ctx, f.repo, stash.ID, winner.Revision, p); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal("conflicting pop overwrote newer entry", err)
	}
	pending, err := s.ListIndexStashes(ctx, f.repo)
	if err != nil || len(pending) != 1 {
		t.Fatal("failed pop consumed stash", pending, err)
	}
	empty = winner
	empty.Sequence++
	empty.Entries = []domain.StagedSession{}
	empty = empty.WithRevision()
	if err := s.CompareAndSwapStaging(ctx, winner.Revision, empty, p); err != nil {
		t.Fatal(err)
	}
	restored, err := s.PopIndex(ctx, f.repo, stash.ID, empty.Revision, p)
	if err != nil || !reflect.DeepEqual(restored.Entries, f.index.Entries) {
		t.Fatal(restored, err)
	}
	pending, err = s.ListIndexStashes(ctx, f.repo)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	replay, err := s.PopIndex(ctx, f.repo, stash.ID, restored.Revision, p)
	if err != nil || replay.Revision != restored.Revision {
		t.Fatal("pop was not idempotent", replay, err)
	}
	// Even missing/corrupt index state cannot make a saved receipt invisible to GC.
	if err := os.Remove(s.stagingPath()); err != nil {
		t.Fatal(err)
	}
	if pinned, err := s.HasStagingPin(ctx, f.op.Ref.Target); err != nil || !pinned {
		t.Fatal("missing index hid stash pin", pinned, err)
	}
}

func TestStagingDeletionRefusesFrozenAndSavedManifestReferences(t *testing.T) {
	f := newFrozenFixture(t)
	ctx := context.Background()
	for _, remove := range []func(context.Context, domain.ContentHash) error{f.store.DeleteSnapshot, f.store.DeleteDoc} {
		if err := remove(ctx, f.op.Ref.Target); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatal("staged reference was deletable", err)
		}
	}
	stash := domain.StagingStash{Version: domain.StagingVersion, ID: strings.Repeat("d", 32), Index: f.index, Position: f.position, CreatedAt: time.Now().UTC()}
	if _, err := f.store.StashIndex(ctx, stash); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(context.Context, domain.ContentHash) error{f.store.DeleteSnapshot, f.store.DeleteDoc} {
		if err := remove(ctx, f.op.Ref.Target); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatal("saved reference was deletable", err)
		}
	}
}

func TestStagingAcceptedManifestSurvivesUnstageAndReadd(t *testing.T) {
	for _, change := range []string{"unstage", "readd"} {
		t.Run(change, func(t *testing.T) {
			f := newFrozenFixture(t)
			s := f.store
			ctx := context.Background()
			if err := s.writeStagingOperation(f.op); err != nil {
				t.Fatal(err)
			}
			next := f.index
			next.Sequence++
			next.Entries = append([]domain.StagedSession{}, next.Entries...)
			if change == "unstage" {
				next.Entries = []domain.StagedSession{}
			} else {
				next.Entries[0].CapturedAt = next.Entries[0].CapturedAt.Add(time.Second)
			}
			next = next.WithRevision()
			if err := s.CompareAndSwapStaging(ctx, f.index.Revision, next, f.position); err != nil {
				t.Fatal(err)
			}
			op, err := s.ResumeStagingCommit(ctx, f.repo, f.op.ID)
			if err != nil || !op.LocalFinalized {
				t.Fatal("accepted commit canceled by next index", op, err)
			}
			actual, p, err := s.ReadStaging(ctx, f.repo)
			if err != nil || actual.Revision != next.Revision || p.Snapshot != f.op.Ref.Target {
				t.Fatal("accepted operation consumed future index", actual, p, err)
			}
		})
	}
}
