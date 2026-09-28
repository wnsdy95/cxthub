package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestBackfillRestartPinsAndAcknowledgmentFence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := NewFileStore(root)
	repo := string(domain.HashContent([]byte("queue-repository")))
	id := domain.HashContent([]byte("archive"))
	snap := domain.Snapshot{RepoID: repo, ID: id, DocHash: id, Message: "first"}
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	restarted := NewFileStore(root)
	jobs, err := restarted.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 1 {
		t.Fatal("queue did not survive reopen", jobs, err)
	}
	old := jobs[0]
	if pin, err := restarted.HasBackfillPin(ctx, id); err != nil || !pin {
		t.Fatal("missing collection pin", err)
	}
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	jobs, _ = s.ListBackfills(ctx, repo)
	if jobs[0] != old {
		t.Fatal("ordinary retry reset queue fairness/backoff")
	}
	snap.Message = "changed projection"
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.UpdateBackfill(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	jobs, err = restarted.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 1 || jobs[0].StateHash == old.StateHash {
		t.Fatal("delayed acknowledgment removed new work", jobs, err)
	}
	current := jobs[0]
	next := current
	next.Version++
	next.Attempts++
	next.Reason = "unavailable"
	next.NextAttempt = time.Now().Add(time.Minute).UTC()
	if err := s.UpdateBackfill(ctx, current, &next); err != nil {
		t.Fatal(err)
	}
	jobs, _ = restarted.ListBackfills(ctx, repo)
	if len(jobs) != 1 || jobs[0] != next {
		t.Fatal("retry state not durable", jobs)
	}
	if err := s.UpdateBackfill(ctx, next, nil); err != nil {
		t.Fatal(err)
	}
	if pin, err := restarted.HasBackfillPin(ctx, id); err != nil || pin {
		t.Fatal("completed pin remains", err)
	}
}

func TestBackfillCorruptionAndWorkerExclusion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, peer := NewFileStore(root), NewFileStore(root)
	_, err := s.WithBackfillWorker(ctx, func() error {
		entered, err := peer.WithBackfillWorker(ctx, func() error { t.Fatal("duplicate worker entered"); return nil })
		if err != nil || entered {
			t.Fatal(entered, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, err := peer.WithBackfillWorker(ctx, func() error { return nil })
	if err != nil || !entered {
		t.Fatal("worker lock leaked", entered, err)
	}
	repo, id := string(domain.HashContent([]byte("repo"))), domain.HashContent([]byte("doc"))
	snap := domain.Snapshot{RepoID: repo, ID: id, DocHash: id}
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	path, _ := s.backfillPath(repo, id)
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ListBackfills(ctx, repo); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("corrupt queue ignored", err)
	}
	if _, err := peer.HasBackfillPin(ctx, id); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("corruption permitted collection", err)
	}
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("corrupt queue overwritten", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := s.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err == nil {
		t.Fatal("queue followed symlink")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "untouched" {
		t.Fatal("wrote outside replica")
	}
}
