package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRemoteSnapshotStateCursorRoundTripAndValidation(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("cursor-repo")))
	id := domain.HashContent([]byte("cursor-snapshot"))
	local := domain.HashContent([]byte("cursor-local"))
	remote := domain.HashContent([]byte("cursor-remote"))

	if err := store.SaveRemoteSnapshotStateCursor(ctx, repoID, map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{
		id: {LocalState: local, RemoteState: remote},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadRemoteSnapshotStateCursor(ctx, repoID)
	if err != nil || len(got) != 1 || got[id].LocalState != local || got[id].RemoteState != remote {
		t.Fatalf("cursor=%+v err=%v", got, err)
	}

	path, err := store.remoteSnapshotStateCursorPath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"repo_id":"wrong","entries":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRemoteSnapshotStateCursor(ctx, repoID); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt cursor error=%v", err)
	}

	if err := store.SaveRemoteSnapshotStateCursor(ctx, repoID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("empty cursor file still exists: %v", err)
	}
}

func TestRemoteSnapshotStateCursorRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".cxt", "remote-state-cursors")); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(root)
	repoID := string(domain.HashContent([]byte("symlink-cursor-repo")))
	id := domain.HashContent([]byte("symlink-cursor-snapshot"))
	err := store.SaveRemoteSnapshotStateCursor(context.Background(), repoID, map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{
		id: {LocalState: domain.HashContent([]byte("local")), RemoteState: domain.HashContent([]byte("remote"))},
	})
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("symlinked cursor directory error=%v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cursor escaped .cxt: entries=%v err=%v", entries, err)
	}
}
