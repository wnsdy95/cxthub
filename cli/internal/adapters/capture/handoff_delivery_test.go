package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

func handoffDeliveryRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := providerfs.WriteRepoFileAtomic(repo, ".cxt/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestDeliverSessionHandoffRetriesAndIsolatesSessions(t *testing.T) {
	repo := handoffDeliveryRepo(t)
	for _, id := range []string{"first", "second"} {
		if err := WriteSessionHandoff(repo, []string{id}, id+" old text"); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	if delivered, err := DeliverSessionHandoff(context.Background(), repo, "unrelated", func() error { calls++; return nil }); delivered || err != nil || calls != 0 {
		t.Fatalf("unrelated session delivered: %v %v calls=%d", delivered, err, calls)
	}
	relative := handoffRelativePath("session\x00first")
	before, _ := providerfs.ReadRepoFile(repo, relative)
	failure := errors.New("delivery failed")
	if delivered, err := DeliverSessionHandoff(context.Background(), repo, "first", func() error { calls++; return failure }); delivered || !errors.Is(err, failure) {
		t.Fatalf("failed delivery acknowledged: %v %v", delivered, err)
	}
	after, err := providerfs.ReadRepoFile(repo, relative)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed delivery changed queue: %v", err)
	}
	if delivered, err := DeliverSessionHandoff(context.Background(), repo, "first", func() error { calls++; return nil }); !delivered || err != nil {
		t.Fatalf("retry failed: %v %v", delivered, err)
	}
	if delivered, err := DeliverSessionHandoff(context.Background(), repo, "first", func() error { calls++; return nil }); delivered || err != nil || calls != 2 {
		t.Fatalf("acknowledged generation repeated: %v %v calls=%d", delivered, err, calls)
	}
	if text, ok := ConsumeSessionHandoff(repo, "second"); !ok || text != "second old text" {
		t.Fatalf("other session changed: %q %v", text, ok)
	}
}

func TestDeliverSessionHandoffPreservesConcurrentReplacement(t *testing.T) {
	for _, kind := range []string{"new body", "identical bytes", "failed callback"} {
		t.Run(kind, func(t *testing.T) {
			repo := handoffDeliveryRepo(t)
			if err := WriteSessionHandoff(repo, []string{"session"}, "original"); err != nil {
				t.Fatal(err)
			}
			relative := handoffRelativePath("session\x00session")
			original, _ := providerfs.ReadRepoFile(repo, relative)
			failure := errors.New("write failed after replacement")
			delivered, err := DeliverSessionHandoff(context.Background(), repo, "session", func() error {
				// Writers can replace the request while delivery is in progress.
				if kind == "identical bytes" {
					return withBriefingFileLock(repo, relative, func() error {
						return providerfs.WriteRepoFileAtomic(repo, relative, original, 0o644)
					})
				}
				if err := WriteSessionHandoff(repo, []string{"session"}, "replacement"); err != nil {
					return err
				}
				if kind == "failed callback" {
					return failure
				}
				return nil
			})
			if kind == "failed callback" {
				if delivered || !errors.Is(err, failure) {
					t.Fatalf("failed callback acknowledged: %v %v", delivered, err)
				}
			} else if !delivered || err != nil {
				t.Fatalf("delivery failed: %v %v", delivered, err)
			}
			want := "replacement"
			if kind == "identical bytes" {
				want = "original"
			}
			if text, ok := ConsumeSessionHandoff(repo, "session"); !ok || text != want {
				t.Fatalf("replacement deleted: %q %v", text, ok)
			}
		})
	}
}

func TestDeliverSessionHandoffConcurrentConsumersDeliverOnce(t *testing.T) {
	repo := handoffDeliveryRepo(t)
	if err := WriteSessionHandoff(repo, nil, "worktree request"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	start, entered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		go func() {
			<-start
			_, err := DeliverSessionHandoff(context.Background(), repo, id, func() error {
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release
				return nil
			})
			results <- err
		}()
	}
	close(start)
	<-entered
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("worktree fallback delivered %d times", calls.Load())
	}
}

func TestDeliverSessionHandoffCancellationAndUnsafePath(t *testing.T) {
	repo := handoffDeliveryRepo(t)
	if err := WriteSessionHandoff(repo, []string{"session"}, "pending"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	callback := func() error { t.Error("invalid delivery called back"); return nil }
	if delivered, err := DeliverSessionHandoff(ctx, repo, "session", callback); delivered || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delivery: %v %v", delivered, err)
	}
	if text, ok := ConsumeSessionHandoff(repo, "session"); !ok || text != "pending" {
		t.Fatal("canceled request lost")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, handoffRelativePath("session\x00session"))); err != nil {
		t.Fatal(err)
	}
	if delivered, err := DeliverSessionHandoff(context.Background(), repo, "session", callback); delivered || err == nil {
		t.Fatalf("symlink accepted: %v %v", delivered, err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "untouched" {
		t.Fatal("symlink target changed", err)
	}
}
