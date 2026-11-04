package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type fsJoinProjectionImage struct {
	path string
	data []byte
}

func fsJoinProjectionFixture(test *testing.T) (*FSStore, domain.JoinMutation, fsJoinJournal) {
	test.Helper()
	ctx := context.Background()
	storage, repoID, head, source, before, after := joinRecoveryFixture(test)
	tip := domain.HashContent([]byte("join-projection-tip"))
	if err := storage.PutSnapshot(ctx, domain.Snapshot{
		ID: tip, DocHash: tip, RepoID: repoID, Branch: "main", Parents: []domain.ContentHash{source},
	}); err != nil {
		test.Fatal(err)
	}
	branchID := "join-projection-branch"
	if err := storage.CompareAndSwapRef(ctx, repoID, domain.Ref{
		RepoID: repoID, Kind: domain.RefBranch, Name: "main", BranchID: branchID, Target: head,
	}, head); err != nil {
		test.Fatal(err)
	}
	if err := storage.CompareAndSwapRef(ctx, repoID, domain.Ref{
		RepoID: repoID, Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "projection-source", Target: tip,
	}, ""); err != nil {
		test.Fatal(err)
	}
	mutation := domain.JoinMutation{
		RepoID: repoID, Branch: "main", BranchID: branchID, Source: source, Segment: []domain.ContentHash{source, tip},
		ExpectedHead: head, NewHead: source, ForkName: domain.SessionRefPrefix("main") + "projection-fork", ForkTip: tip,
		Grafts: []domain.GraftPatch{{SnapshotID: source, ExpectedSeq: 0, Parents: []domain.ContentHash{head}}},
	}
	journal := fsJoinJournal{
		Version: fsJoinJournalVersion, Phase: fsJoinCommitted, RepoID: repoID,
		Branch: mutation.Branch, BranchID: branchID, ExpectedHead: head, NewHead: source,
		ForkName: mutation.ForkName, ForkTip: tip, CreatedAt: time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC),
		Snapshots: []fsJoinSnapshot{{ID: source, Before: before, After: after}},
	}
	return storage, mutation, journal
}

func fsJoinProjectionImages(storage *FSStore, journal fsJoinJournal) []fsJoinProjectionImage {
	images := make([]fsJoinProjectionImage, 0, len(journal.Snapshots)+2)
	for _, snapshot := range journal.Snapshots {
		images = append(images, fsJoinProjectionImage{path: storage.snapshotPath(journal.RepoID, snapshot.ID), data: snapshot.After})
	}
	if journal.ForkName != "" {
		images = append(images, fsJoinProjectionImage{path: storage.refFile(journal.RepoID, domain.RefSession, journal.ForkName), data: []byte(string(journal.ForkTip) + "\n")})
	}
	images = append(images,
		fsJoinProjectionImage{path: storage.refFile(journal.RepoID, domain.RefBranch, journal.Branch), data: encodeRef(domain.Ref{
			RepoID: journal.RepoID, Kind: domain.RefBranch, Name: journal.Branch, BranchID: journal.BranchID, Target: journal.NewHead,
		})},
	)
	return images
}

func trackFSJoinProjectionWrites(test *testing.T, storage *FSStore, images []fsJoinProjectionImage) map[string]int {
	test.Helper()
	expected := make(map[string][]byte, len(images))
	for _, image := range images {
		expected[image.path] = image.data
	}
	writes := make(map[string]int, len(images))
	storage.joinProjectionWriter = func(path string, data []byte) error {
		want, ok := expected[path]
		if !ok {
			test.Fatalf("non-projection write through projection writer: %s", path)
		}
		if !bytes.Equal(data, want) {
			test.Fatalf("projection %s wrote %q, want after-image %q", path, data, want)
		}
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		writes[path]++
		return nil
	}
	return writes
}

func assertFSJoinProjectionWrites(test *testing.T, images []fsJoinProjectionImage, writes map[string]int, want int) {
	test.Helper()
	for _, image := range images {
		if writes[image.path] != want {
			test.Errorf("projection %s delegated actual writes=%d, want %d", image.path, writes[image.path], want)
		}
		data, err := os.ReadFile(image.path)
		if err != nil || !bytes.Equal(data, image.data) {
			test.Errorf("projection %s = %q, err=%v, want after-image %q", image.path, data, err, image.data)
		}
	}
}

func fsJoinProjectionReflogs(journal fsJoinJournal) []domain.RefLogEntry {
	var entries []domain.RefLogEntry
	if journal.ForkName != "" {
		entries = append(entries, domain.RefLogEntry{Kind: domain.RefSession, Name: journal.ForkName, New: journal.ForkTip, CreatedAt: journal.CreatedAt})
	}
	return append(entries,
		domain.RefLogEntry{Kind: domain.RefBranch, Name: journal.Branch, Old: journal.ExpectedHead, New: journal.NewHead, CreatedAt: journal.CreatedAt},
	)
}

func assertFSJoinProjectionReflogs(test *testing.T, storage *FSStore, journal fsJoinJournal, want int) {
	test.Helper()
	logs, err := storage.ReadReflog(context.Background(), journal.RepoID)
	if err != nil {
		test.Fatal(err)
	}
	for _, expected := range fsJoinProjectionReflogs(journal) {
		count := 0
		for _, entry := range logs {
			if entry.Kind == expected.Kind && entry.Name == expected.Name && entry.Old == expected.Old && entry.New == expected.New {
				count++
				if !entry.CreatedAt.Equal(expected.CreatedAt) {
					test.Errorf("join reflog %s timestamp=%v, want %v", entry.Name, entry.CreatedAt, expected.CreatedAt)
				}
			}
		}
		if count != want {
			test.Errorf("join reflog %s count=%d, want %d", expected.Name, count, want)
		}
	}
}

func TestFSStoreApplyJoinWritesEachProjectionOnce(test *testing.T) {
	storage, mutation, journal := fsJoinProjectionFixture(test)
	images := fsJoinProjectionImages(storage, journal)
	writes := trackFSJoinProjectionWrites(test, storage, images)
	if err := storage.ApplyJoin(context.Background(), mutation); err != nil {
		test.Fatal(err)
	}
	assertFSJoinProjectionWrites(test, images, writes, 1)
	if _, err := os.Stat(storage.joinJournalPath(journal.RepoID)); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("successful join retained journal: %v", err)
	}
}

func TestFSStoreApplyJoinCleanupFailurePreservesCommittedRecovery(test *testing.T) {
	storage, mutation, journal := fsJoinProjectionFixture(test)
	assertFSJoinCleanupFailureRecovery(test, storage, journal, func() error { return storage.ApplyJoin(context.Background(), mutation) })
}

func TestFSStoreAppendRefWritesEachProjectionOnce(test *testing.T) {
	storage, mutation, journal := fsJoinProjectionFixture(test)
	journal.ForkName, journal.ForkTip = "", ""
	images := fsJoinProjectionImages(storage, journal)
	writes := trackFSJoinProjectionWrites(test, storage, images)
	if err := storage.AppendRef(context.Background(), mutation.RepoID, domain.Ref{Kind: domain.RefBranch, Name: mutation.Branch, BranchID: mutation.BranchID, Target: mutation.NewHead}, mutation.ExpectedHead); err != nil {
		test.Fatal(err)
	}
	assertFSJoinProjectionWrites(test, images, writes, 1)
	if _, err := os.Stat(storage.joinJournalPath(journal.RepoID)); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("successful append retained journal", err)
	}
}

func TestFSStoreAppendRefCleanupFailurePreservesCommittedRecovery(test *testing.T) {
	storage, mutation, journal := fsJoinProjectionFixture(test)
	journal.ForkName, journal.ForkTip = "", ""
	assertFSJoinCleanupFailureRecovery(test, storage, journal, func() error {
		return storage.AppendRef(context.Background(), mutation.RepoID, domain.Ref{Kind: domain.RefBranch, Name: mutation.Branch, BranchID: mutation.BranchID, Target: mutation.NewHead}, mutation.ExpectedHead)
	})
}

func assertFSJoinCleanupFailureRecovery(test *testing.T, storage *FSStore, journal fsJoinJournal, apply func() error) {
	test.Helper()
	images := fsJoinProjectionImages(storage, journal)
	writes := trackFSJoinProjectionWrites(test, storage, images)
	reflogPath := storage.reflogPath(journal.RepoID)
	if err := os.Remove(reflogPath); err != nil {
		test.Fatal(err)
	}
	if err := os.Mkdir(reflogPath, 0700); err != nil {
		test.Fatal(err)
	}
	if err := apply(); err == nil {
		test.Fatal("join ignored the post-commit reflog failure")
	}
	assertFSJoinProjectionWrites(test, images, writes, 1)
	journalPath := storage.joinJournalPath(journal.RepoID)
	raw, err := os.ReadFile(journalPath)
	if err != nil || json.Unmarshal(raw, &journal) != nil || journal.Phase != fsJoinCommitted {
		test.Fatal("cleanup failure removed or rolled back committed journal", err)
	}
	if err := os.Remove(reflogPath); err != nil {
		test.Fatal(err)
	}
	clear(writes)
	if err := storage.recoverJoinJournals(); err != nil {
		test.Fatal(err)
	}
	assertFSJoinProjectionWrites(test, images, writes, 1)
	assertFSJoinProjectionReflogs(test, storage, journal, 1)
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("committed recovery retained a completed journal", err)
	}
}

func TestFSStoreCommittedJoinRecoveryReplaysEveryProjection(test *testing.T) {
	states := []struct {
		name        string
		afterImages int
	}{
		{name: "before", afterImages: 0},
		{name: "mixed", afterImages: 1},
		{name: "all-after", afterImages: 3},
	}
	for _, state := range states {
		test.Run(state.name, func(test *testing.T) {
			storage, _, journal := fsJoinProjectionFixture(test)
			images := fsJoinProjectionImages(storage, journal)
			for _, image := range images[:state.afterImages] {
				if err := writeAtomic(image.path, image.data); err != nil {
					test.Fatal(err)
				}
			}
			if state.afterImages == len(images) {
				for _, entry := range fsJoinProjectionReflogs(journal) {
					if err := storage.appendReflogStrict(journal.RepoID, entry); err != nil {
						test.Fatal(err)
					}
				}
			}
			journalPath := storage.joinJournalPath(journal.RepoID)
			if err := storage.writeJoinJournal(journalPath, journal); err != nil {
				test.Fatal(err)
			}
			recovered := &FSStore{dataDir: storage.dataDir}
			writes := trackFSJoinProjectionWrites(test, recovered, images)
			if err := recovered.recoverJoinJournals(); err != nil {
				test.Fatal(err)
			}
			assertFSJoinProjectionWrites(test, images, writes, 1)
			assertFSJoinProjectionReflogs(test, recovered, journal, 1)
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				test.Fatalf("recovered join retained journal: %v", err)
			}
			if err := recovered.recoverJoinJournals(); err != nil {
				test.Fatal(err)
			}
			assertFSJoinProjectionWrites(test, images, writes, 1)
			assertFSJoinProjectionReflogs(test, recovered, journal, 1)
		})
	}
}

func TestFSStoreCommittedJoinRecoveryRetriesProjectionFailure(test *testing.T) {
	for failureIndex, name := range []string{"snapshot", "fork", "branch"} {
		test.Run(name, func(test *testing.T) {
			storage, _, journal := fsJoinProjectionFixture(test)
			images := fsJoinProjectionImages(storage, journal)
			journalPath := storage.joinJournalPath(journal.RepoID)
			if err := storage.writeJoinJournal(journalPath, journal); err != nil {
				test.Fatal(err)
			}
			before, err := os.ReadFile(journalPath)
			if err != nil {
				test.Fatal(err)
			}
			recovered := &FSStore{dataDir: storage.dataDir}
			writes := trackFSJoinProjectionWrites(test, recovered, images)
			delegate := recovered.joinProjectionWriter
			injected := errors.New("injected join projection write failure")
			failures := 0
			recovered.joinProjectionWriter = func(path string, data []byte) error {
				if path == images[failureIndex].path {
					failures++
					return injected
				}
				return delegate(path, data)
			}
			if err := recovered.recoverJoinJournals(); !errors.Is(err, injected) {
				test.Fatalf("recovery err=%v, want injected projection failure", err)
			}
			if failures != 1 {
				test.Fatalf("injected projection failures=%d, want 1", failures)
			}
			after, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(after, before) {
				test.Fatalf("failed recovery changed or removed committed journal: %q, err=%v", after, err)
			}
			for imageIndex, image := range images {
				want := 0
				if imageIndex < failureIndex {
					want = 1
				}
				if writes[image.path] != want {
					test.Errorf("failed recovery projection %s writes=%d, want %d", image.path, writes[image.path], want)
				}
			}
			assertFSJoinProjectionReflogs(test, recovered, journal, 0)
			recovered.joinProjectionWriter = delegate
			clear(writes)
			if err := recovered.recoverJoinJournals(); err != nil {
				test.Fatal(err)
			}
			assertFSJoinProjectionWrites(test, images, writes, 1)
			assertFSJoinProjectionReflogs(test, recovered, journal, 1)
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				test.Fatalf("successful recovery retry retained journal: %v", err)
			}
			if err := recovered.recoverJoinJournals(); err != nil {
				test.Fatal(err)
			}
			assertFSJoinProjectionWrites(test, images, writes, 1)
			assertFSJoinProjectionReflogs(test, recovered, journal, 1)
		})
	}
}
