package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// The baseline has no admission capability and leaves the cursor unchanged.
// Keeping this fallback permits the same positive regression to run against it.
func ensureCapturePositionForTest(ctx context.Context, s *FileStore, repo string) error {
	if admission, ok := any(s).(interface {
		EnsureCapturePosition(context.Context, string) error
	}); ok {
		return admission.EnsureCapturePosition(ctx, repo)
	}
	return nil
}

func captureAdmissionFixture(t *testing.T) (*FileStore, domain.WorkingPosition, string) {
	t.Helper()
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("b", 40))
	old := domain.WorkingPosition{RepoID: string(domain.HashContent([]byte("git-origin"))), Branch: "main", GitCommit: strings.Repeat("a", 40)}
	if err := s.PutWorkingPosition(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, old, string(domain.HashContent([]byte("connected-server")))
}

func admissionPending(t *testing.T, s *FileStore, repo, session string) domain.Snapshot {
	t.Helper()
	ctx := context.Background()
	cir := sampleCIR("test-owned pending " + session)
	cir.Envelope.SessionOriginID = session
	id, err := s.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.PutSettingsObject(ctx, domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{}})
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ClaudeSettings: settings, ID: id, DocHash: id, RepoID: repo, Branch: "main", SessionID: session, Provider: domain.ProviderClaude, Message: domain.HookMessagePrefix + "pending", Parents: []domain.ContentHash{}}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPending(ctx, domain.Pending{RepoID: repo, Branch: snap.Branch, SessionID: session, Provider: snap.Provider, Target: id}); err != nil {
		t.Fatal(err)
	}
	return snap
}

func admissionBytes(t *testing.T, s *FileStore) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(s.storeDir(), func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.storeDir(), path)
		if err != nil {
			return err
		}
		if e.IsDir() && rel == "locks" {
			return filepath.SkipDir
		}
		if e.IsDir() {
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			result[rel] = "symlink"
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCaptureAdmissionConnectedPendingAndOldCode(t *testing.T) {
	s, old, repo := captureAdmissionFixture(t)
	admissionPending(t, s, repo, "one")
	admissionPending(t, s, repo, "two")
	// Real pending capture also retains settings and provider bookkeeping.
	if _, err := s.PutSettingsObject(context.Background(), domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"capture/claude-fixture.cursor", "capture/claude-fixture.last", "capture/claude-fixture.observer", "capture/projections/fixture.json", "app-sessions/registry.lock", "session-affinity/fixture.json"} {
		if err := writeAtomic(filepath.Join(s.storeDir(), path), []byte("test-owned metadata")); err != nil {
			t.Fatal(err)
		}
	}
	// Another empty worktree already belongs to the connected repository.
	peer := NewWorktreeFileStore(s.repoRoot, filepath.Join(s.repoRoot, ".git", "worktrees", "peer"), "other", strings.Repeat("c", 40))
	if err := peer.PutWorkingPosition(context.Background(), domain.WorkingPosition{RepoID: repo, Branch: "other", GitCommit: peer.gitCommit}); err != nil {
		t.Fatal(err)
	}
	before := admissionBytes(t, s)
	if err := ensureCapturePositionForTest(context.Background(), s, repo); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkingPosition(context.Background())
	want := old
	want.RepoID = repo
	want.BranchID = domain.LegacyContextBranchID(repo, want.Branch)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("pending-before-capture normalized cursor = %+v, want %+v; %v", got, want, err)
	}
	after := admissionBytes(t, s)
	position, _ := filepath.Rel(s.storeDir(), s.positionPath())
	delete(before, position)
	delete(after, position)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("normalization changed pending bytes, HEAD, or another worktree")
	}
	if got.GitCommit == s.gitCommit {
		t.Fatal("invented a current-code binding")
	}
	if err := ensureCapturePositionForTest(context.Background(), s, repo); err != nil {
		t.Fatal("retry", err)
	}
}

func TestCaptureAdmissionRejectsEvidenceWithoutWrites(t *testing.T) {
	for _, kind := range []string{"snapshot", "shared", "memory", "pinned", "selection", "orphan", "rewound", "alias", "modern-id", "wrong-branch", "foreign-pending", "unaccounted-snapshot", "unknown-snapshot-file", "extra-doc", "corrupt-pending", "unknown-pending", "pending-target-mismatch", "pending-ancestry", "foreign-memory", "ref", "history", "binding", "capture-intent", "staging", "foreign-worktree", "selected-worktree", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, old, repo := captureAdmissionFixture(t)
			snap := admissionPending(t, s, repo, "one")
			p := old
			path := ""
			switch kind {
			case "snapshot":
				p.Snapshot = snap.ID
			case "shared":
				p.SharedTarget = snap.ID
			case "memory":
				p.MemoryHash = domain.HashContent([]byte("memory"))
			case "pinned":
				p.MemoryPinned = true
			case "selection":
				p.Selection = &domain.HistoryEvent{}
			case "orphan":
				p.Orphan = true
			case "rewound":
				p.Rewound = true
			case "alias":
				p.LocalBranch = "main"
			case "modern-id":
				p.BranchID = "modern-identity"
			case "wrong-branch":
				s.gitBranch = "other"
			case "foreign-pending":
				admissionPending(t, s, old.RepoID, "foreign")
			case "unaccounted-snapshot":
				extra := admissionPending(t, s, repo, "unaccounted")
				if err := os.Remove(s.pendingPath(extra.SessionID)); err != nil {
					t.Fatal(err)
				}
			case "unknown-snapshot-file":
				path = filepath.Join(s.storeDir(), "objects", "snapshots", "ignored")
			case "extra-doc":
				if _, err := s.PutDoc(context.Background(), domain.SessionDoc{CIR: sampleCIR("unaccounted")}); err != nil {
					t.Fatal(err)
				}
			case "corrupt-pending":
				path = s.pendingPath("one")
			case "unknown-pending":
				path = filepath.Join(s.storeDir(), "pending", "ignored")
			case "pending-target-mismatch":
				snap.SessionID = "different"
				b, _ := json.Marshal(snap)
				if err := writeAtomic(s.objectPath("snapshots", snap.ID), b); err != nil {
					t.Fatal(err)
				}
			case "pending-ancestry":
				snap.Parents = []domain.ContentHash{domain.HashContent([]byte("old ancestry"))}
				b, _ := json.Marshal(snap)
				if err := writeAtomic(s.objectPath("snapshots", snap.ID), b); err != nil {
					t.Fatal(err)
				}
			case "foreign-memory":
				if _, err := s.PutMemory(context.Background(), domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("foreign owner")), Provider: domain.ProviderClaude}); err != nil {
					t.Fatal(err)
				}
			case "ref":
				path = filepath.Join(s.storeDir(), "refs", "heads", "other")
			case "history":
				path = filepath.Join(s.storeDir(), "history", "hidden.json")
			case "binding":
				path = filepath.Join(s.storeDir(), "branch-bindings", "other.json")
			case "capture-intent":
				path = filepath.Join(filepath.Dir(s.positionPath()), "capture-passes", "intent.json")
			case "staging":
				path = filepath.Join(filepath.Dir(s.positionPath()), "index.json")
			case "foreign-worktree", "selected-worktree":
				peer := NewWorktreeFileStore(s.repoRoot, filepath.Join(s.repoRoot, ".git", "worktrees", "peer"), "other", s.gitCommit)
				other := domain.WorkingPosition{WorktreeID: peer.worktreeID, RepoID: repo, Branch: "other", GitCommit: s.gitCommit}
				if kind == "foreign-worktree" {
					other.RepoID = old.RepoID
				} else {
					other.Snapshot = snap.ID
				}
				other.BranchID = domain.LegacyContextBranchID(other.RepoID, other.Branch)
				b, _ := json.Marshal(other)
				if err := writeAtomic(peer.positionPath(), b); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(s.objectPath("snapshots", snap.ID), filepath.Join(s.storeDir(), "pending", "alias.json")); err != nil {
					t.Fatal(err)
				}
			}
			if path != "" {
				if err := writeAtomic(path, []byte("{}")); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(p, old) {
				b, _ := json.Marshal(p)
				if err := writeAtomic(s.positionPath(), b); err != nil {
					t.Fatal(err)
				}
			}
			before := admissionBytes(t, s)
			if err := ensureCapturePositionForTest(context.Background(), s, repo); err == nil {
				t.Fatal("unsafe admission accepted")
			}
			if !reflect.DeepEqual(before, admissionBytes(t, s)) {
				t.Fatal("rejected admission changed durable evidence")
			}
		})
	}
}

func TestCaptureAdmissionNoopAndNestedGate(t *testing.T) {
	s, old, repo := captureAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.WithCaptureTrackingGate(ctx, func(locked context.Context) error {
		if err := ensureCapturePositionForTest(locked, s, old.RepoID); err != nil {
			return err
		}
		err := ensureCapturePositionForTest(locked, s, repo)
		if err == nil {
			return fmt.Errorf("nested mismatch accepted")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("attempted lock upgrade")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.positionPath()); err != nil {
		t.Fatal(err)
	}
	before := admissionBytes(t, s)
	if err := ensureCapturePositionForTest(ctx, s, repo); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, admissionBytes(t, s)) {
		t.Fatal("no-position admission initialized a cursor")
	}
	if err := ensureCapturePositionForTest(ctx, s, "invalid"); err == nil {
		t.Fatal("invalid requested identity accepted")
	}
}

func TestCaptureAdmissionWaitsForCaptureAndRechecks(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			s, old, repo := captureAdmissionFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- s.WithCaptureTrackingGate(context.Background(), func(locked context.Context) error {
					close(entered)
					<-release
					owner := repo
					if foreign {
						owner = old.RepoID
					}
					// Only the pending write needs the nested gate; fixture data was
					// prepared before admission below to avoid test goroutine Fatals.
					return s.PutPending(locked, domain.Pending{RepoID: owner, Branch: "main", SessionID: "racer", Provider: domain.ProviderClaude, Target: domain.HashContent([]byte("not completed"))})
				})
			}()
			<-entered
			limited, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			err := ensureCapturePositionForTest(limited, s, repo)
			cancel()
			close(release)
			writerErr := <-done
			if !errors.Is(err, context.DeadlineExceeded) || writerErr != nil {
				t.Fatalf("capture exclusion: %v / %v", err, writerErr)
			}
			before := admissionBytes(t, s)
			if err := ensureCapturePositionForTest(context.Background(), s, repo); err == nil {
				t.Fatal("incomplete or foreign capture accepted")
			}
			if !reflect.DeepEqual(before, admissionBytes(t, s)) {
				t.Fatal("capture changed on failure")
			}
			if err := s.WithCaptureTrackingGate(context.Background(), func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureAdmissionConcurrentIdentical(t *testing.T) {
	s, _, repo := captureAdmissionFixture(t)
	admissionPending(t, s, repo, "one")
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- ensureCapturePositionForTest(context.Background(), s, repo) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.GetWorkingPosition(context.Background())
	if err != nil || p.RepoID != repo {
		t.Fatal("not normalized", err)
	}
}

func TestCaptureAdmissionSnapshotWriterExclusion(t *testing.T) {
	s, _, repo := captureAdmissionFixture(t)
	admissionPending(t, s, repo, "one")
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.withOSLock(context.Background(), "first-tracking", "snapshots", syscall.LOCK_SH, true, func() error { close(held); <-release; return nil })
		done <- err
	}()
	<-held
	limited, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := ensureCapturePositionForTest(limited, s, repo)
	cancel()
	close(release)
	lockErr := <-done
	if !errors.Is(err, context.DeadlineExceeded) || lockErr != nil {
		t.Fatalf("snapshot exclusion: %v / %v", err, lockErr)
	}
	if err := ensureCapturePositionForTest(context.Background(), s, repo); err != nil {
		t.Fatal("lock leaked", err)
	}
}

// Hold the ref writer lock, wait until admission owns capture EX, then change
// the cursor. The admission's pre-lock expectation must not overwrite it.
func TestCaptureAdmissionPreservesConcurrentPosition(t *testing.T) {
	s, old, repo := captureAdmissionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	changed := old
	changed.GitCommit = strings.Repeat("c", 40)
	err := s.withRefMutationLock(ctx, func() error {
		go func() { done <- ensureCapturePositionForTest(ctx, s, repo) }()
		for {
			acquired, err := s.withOSLock(ctx, "first-tracking", "repo", syscall.LOCK_SH, false, func() error { return nil })
			if err != nil {
				return err
			}
			if !acquired {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return s.writePosition(changed)
	})
	admissionErr := <-done
	if err != nil || !errors.Is(admissionErr, domain.ErrSelectionChanged) {
		t.Fatalf("concurrent cursor: %v / %v", err, admissionErr)
	}
	got, err := s.GetWorkingPosition(context.Background())
	if err != nil || !reflect.DeepEqual(got, changed) {
		t.Fatalf("overwrote concurrent cursor: %+v %v", got, err)
	}
}
