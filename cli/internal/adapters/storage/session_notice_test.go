package storage

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionNoticeCursorTwoStoresOneDelivery(t *testing.T) {
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", "")
	peer := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", "")
	scope := domain.SessionNoticeScope{RepoID: string(domain.HashContent([]byte("repo"))), WorktreeID: s.worktreeID, Provider: domain.ProviderCodex, SessionID: "session"}
	id := domain.HashContent([]byte("selection"))
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = peer
			}
			if err := store.WithSessionNoticeCursor(context.Background(), scope, func(last domain.ContentHash) (domain.ContentHash, error) {
				if last != id {
					count.Add(1)
				}
				return id, nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("duplicate output=%d", count.Load())
	}
	fail := errors.New("output failed")
	if err := s.WithSessionNoticeCursor(context.Background(), scope, func(last domain.ContentHash) (domain.ContentHash, error) {
		return domain.HashContent([]byte("next")), fail
	}); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if err := peer.WithSessionNoticeCursor(context.Background(), scope, func(last domain.ContentHash) (domain.ContentHash, error) {
		if last != id {
			t.Fatal("failed write advanced cursor")
		}
		return last, nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestSessionNoticeCursorCancellationCorruptionAndScope(t *testing.T) {
	root := t.TempDir()
	s := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", "")
	scope := domain.SessionNoticeScope{RepoID: string(domain.HashContent([]byte("repo"))), WorktreeID: s.worktreeID, Provider: domain.ProviderCodex, SessionID: "session"}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithSessionNoticeCursor(context.Background(), scope, func(domain.ContentHash) (domain.ContentHash, error) {
			close(entered)
			<-release
			return domain.HashContent([]byte("first")), nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := s.WithSessionNoticeCursor(ctx, scope, func(domain.ContentHash) (domain.ContentHash, error) { t.Error("acquired busy lock"); return "", nil })
	close(release)
	if heldErr := <-done; heldErr != nil {
		t.Fatal(heldErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel=%v", err)
	}
	path := filepath.Join(s.storeDir(), "session-notices", scope.Key()+".json")
	bad := []byte(`{"scope":{}}`)
	if err = os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.WithSessionNoticeCursor(context.Background(), scope, func(domain.ContentHash) (domain.ContentHash, error) {
		t.Fatal("corrupt cursor consumed")
		return "", nil
	}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(bad) {
		t.Fatal("corrupt evidence overwritten")
	}
	scope.WorktreeID = strings.Repeat("f", 32)
	if err = s.WithSessionNoticeCursor(context.Background(), scope, func(domain.ContentHash) (domain.ContentHash, error) {
		t.Fatal("foreign worktree consumed")
		return "", nil
	}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal(err)
	}
}
