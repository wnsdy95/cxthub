package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMutationLockLiveOwnerCannotExpire(t *testing.T) {
	root := t.TempDir()
	st := NewFileStore(root)
	err := st.withMutationLock(context.Background(), "refs", "repo", func() error {
		old := time.Now().Add(-2 * snapshotMutationLockStaleAfter)
		if err := os.Chtimes(filepath.Join(st.storeDir(), "locks", "refs", "repo.lock"), old, old); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		if err := NewFileStore(root).withMutationLock(ctx, "refs", "repo", func() error { t.Error("live lock stolen"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contender=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.withMutationLock(context.Background(), "refs", "repo", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestMutationLockProcessCrash(t *testing.T) {
	if root := os.Getenv("CXT_LOCK_TEST_CHILD"); root != "" {
		st := NewFileStore(root)
		if err := st.withMutationLock(context.Background(), "refs", "repo", func() error {
			if err := os.WriteFile(filepath.Join(root, "ready"), []byte("ready"), 0600); err != nil {
				return err
			}
			<-time.After(time.Hour)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestMutationLockProcessCrash$")
	child.Env = append(os.Environ(), "CXT_LOCK_TEST_CHILD="+root)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := NewFileStore(root).withMutationLock(ctx, "refs", "repo", func() error { return nil }); err != nil {
		t.Fatalf("dead owner blocked recovery: %v", err)
	}
}
