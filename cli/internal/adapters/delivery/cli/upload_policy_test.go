package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func uploadLockRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBulkUploadSlotSerializesUploaders(t *testing.T) {
	root := uploadLockRoot(t)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate gates model separate processes: each opens its own
			// descriptor, which is what flock arbitrates.
			for range 3 {
				err := bulkUploadSlot(root)(context.Background(), func() error {
					now := active.Add(1)
					for {
						old := peak.Load()
						if now <= old || peak.CompareAndSwap(old, now) {
							break
						}
					}
					time.Sleep(5 * time.Millisecond)
					active.Add(-1)
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("concurrent bulk requests=%d, want 1", peak.Load())
	}
}

func TestBackgroundUploadYieldsToForegroundPush(t *testing.T) {
	root := uploadLockRoot(t)
	release, entered := make(chan struct{}), make(chan struct{})
	foreground := make(chan error, 1)
	go func() {
		foreground <- runForegroundUpload(context.Background(), root, time.Minute, nil, func(ctx context.Context) error {
			if _, ok := outbound.UploadPolicyFromContext(ctx); !ok {
				t.Error("foreground push lost its patient policy")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	var ran atomic.Bool
	background := make(chan error, 1)
	go func() {
		background <- backgroundUploadGate(root)(context.Background(), func() error { ran.Store(true); return nil })
	}()
	time.Sleep(2500 * time.Millisecond)
	if ran.Load() {
		t.Fatal("background batch ran during a foreground push")
	}
	close(release)
	if err := <-foreground; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-background:
		if err != nil || !ran.Load() {
			t.Fatalf("background batch did not resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background batch stayed paused after the foreground push")
	}
}

func TestBackgroundUploadGateHonorsCancellation(t *testing.T) {
	root := uploadLockRoot(t)
	release, entered := make(chan struct{}), make(chan struct{})
	go func() {
		_ = runForegroundUpload(context.Background(), root, time.Minute, nil, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	defer close(release)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := backgroundUploadGate(root)(ctx, func() error { t.Fatal("ran while paused"); return nil }); err == nil {
		t.Fatal("paused background batch ignored its deadline")
	}
}

func TestForegroundUploadPrintsResumableChunkProgress(t *testing.T) {
	root := uploadLockRoot(t)
	var out bytes.Buffer
	err := runForegroundUpload(context.Background(), root, time.Minute, &out, func(ctx context.Context) error {
		policy, ok := outbound.UploadPolicyFromContext(ctx)
		if !ok || policy.StallBudget != time.Minute || policy.Gate == nil || policy.Progress == nil {
			t.Fatal("foreground policy incomplete")
		}
		policy.Progress(812, 961)
		policy.Progress(812, 961)
		policy.Progress(961, 961)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "push: uploading document chunks; accepted chunks resume on the next run (812/961)\n" +
		"push: uploading document chunks; accepted chunks resume on the next run (961/961)\n"
	if out.String() != want {
		t.Fatalf("progress output:\n%s", out.String())
	}
}
