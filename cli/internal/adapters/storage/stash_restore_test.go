package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestStashConcurrentWritersAndRestoreCAS(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- NewFileStore(root).StashPush(ctx, "repo", domain.StashEntry{Snapshot: domain.HashContent([]byte(fmt.Sprint(i)))})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store := NewFileStore(root)
	stack, err := store.StashList(ctx, "repo")
	if err != nil || len(stack) != writers {
		t.Fatalf("lost push: %d %v", len(stack), err)
	}
	seen := map[domain.ContentHash]bool{}
	for _, entry := range stack {
		seen[entry.Snapshot] = true
	}
	if len(seen) != writers {
		t.Fatal("duplicated or lost stash")
	}
	newer := domain.StashEntry{Snapshot: domain.HashContent([]byte("arrived during restore"))}
	if err := store.StashPush(ctx, "repo", newer); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndDropStash(ctx, "repo", stack); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale pop accepted: %v", err)
	}
	current, err := store.StashList(ctx, "repo")
	if err != nil || len(current) != writers+1 || current[0] != newer {
		t.Fatalf("stale pop mutated stack: %+v %v", current, err)
	}
	if err := store.CompareAndDropStash(ctx, "repo", current); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndDropStash(ctx, "repo", current); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("same restore acknowledged twice: %v", err)
	}
}
