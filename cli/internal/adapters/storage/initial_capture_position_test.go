package storage

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func initialCapturePositionFixture(t *testing.T) (*FileStore, domain.WorkingPosition, domain.WorkingPosition) {
	t.Helper()
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "owner-feature", strings.Repeat("a", 40))
	old := domain.WorkingPosition{RepoID: string(domain.HashContent([]byte("provisional"))), Branch: "main", LocalBranch: "owner-feature", GitCommit: s.gitCommit}
	if err := s.PutWorkingPosition(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repo := string(domain.HashContent([]byte("verified server")))
	next := domain.WorkingPosition{RepoID: repo, Branch: "owner-feature", BranchID: domain.LegacyContextBranchID(repo, "owner-feature"), WorktreeID: s.worktreeID, GitCommit: s.gitCommit}
	return s, old, next
}
func TestInitializeCapturePositionCASAndAuthority(t *testing.T) {
	for _, kind := range []string{"canonical", "stale-empty", "stale-nil", "nonempty", "wrong-identity", "wrong-code", "nonempty-next", "pinned-next"} {
		t.Run(kind, func(t *testing.T) {
			s, old, next := initialCapturePositionFixture(t)
			ctx := context.Background()
			expected := &old
			switch kind {
			case "stale-empty":
				changed := old
				changed.Branch = "changed"
				if err := s.PutWorkingPosition(ctx, changed); err != nil {
					t.Fatal(err)
				}
			case "stale-nil":
				expected = nil
			case "nonempty":
				changed := old
				changed.Snapshot = domain.HashContent([]byte("captured"))
				if err := s.PutWorkingPosition(ctx, changed); err != nil {
					t.Fatal(err)
				}
			case "wrong-identity":
				next.BranchID = old.BranchID
			case "wrong-code":
				next.GitCommit = strings.Repeat("b", 40)
			case "nonempty-next":
				next.Snapshot = domain.HashContent([]byte("invented"))
			case "pinned-next":
				next.MemoryPinned = true
			}
			before, err := s.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = s.InitializeCapturePosition(ctx, expected, next)
			after, readErr := s.GetWorkingPosition(ctx)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kind == "canonical" {
				if err != nil || !reflect.DeepEqual(after, next) {
					t.Fatalf("canonical migration: %+v %v", after, err)
				}
				events, e := s.ListHistoryEvents(ctx, next.RepoID)
				if e != nil || len(events) != 0 {
					t.Fatalf("created history: %+v %v", events, e)
				}
				if err = s.InitializeCapturePosition(ctx, &after, next); err != nil {
					t.Fatal("exact current retry", err)
				}
			} else if err == nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid initialization mutated selection: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
func TestInitializeCapturePositionWaitsForCaptureAndReleases(t *testing.T) {
	s, old, next := initialCapturePositionFixture(t)
	ctx := context.Background()
	peer := NewWorktreeFileStore(s.repoRoot, filepath.Join(s.repoRoot, ".git"), "owner-feature", s.gitCommit)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- peer.WithCaptureTrackingGate(ctx, func(locked context.Context) error {
			close(entered)
			<-release
			return peer.PutPending(locked, domain.Pending{RepoID: old.RepoID, Branch: "owner-feature", SessionID: "concurrent", Target: domain.HashContent([]byte("captured"))})
		})
	}()
	<-entered
	limited, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	err := s.InitializeCapturePosition(limited, &old, next)
	cancel()
	close(release)
	if captureErr := <-done; captureErr != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture admission: %v %v", err, captureErr)
	}
	if err = s.InitializeCapturePosition(ctx, &old, next); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("pending capture overwritten: %v", err)
	}
	got, err := s.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("failed admission changed position: %+v %v", got, err)
	}
	// Both failure exits released their gates; cooperating capture can still write.
	limited, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = peer.WithCaptureTrackingGate(limited, func(context.Context) error { return nil }); err != nil {
		t.Fatal("gate leaked", err)
	}
}
