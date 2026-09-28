package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestCaptureRecoveryJournalAcknowledgmentIsImmutableAndReplaySafe(t *testing.T) {
	cwd, c, st, repo, _ := publicationFixture(t)
	ctx := context.Background()
	p, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.recordOutcome(ctx, cwd, 0, "", "failed", inbound.SaveOutput{}, errors.New("source unavailable")); err != nil {
		t.Fatal(err)
	}
	j := capturejournal.New(cwd, cwd)
	c.CaptureRecovery = app.NewCaptureRecoveryService(j, st)
	path := filepath.Join(cwd, p.relativePath())
	before, _ := os.ReadFile(path)
	inspect := func(ctx context.Context, _ string) ([]domain.CaptureRecoveryStatus, error) {
		return c.CaptureRecovery.Inspect(ctx, repo)
	}
	var doctor bytes.Buffer
	if err := RunDiagnostics(ctx, cwd, []string{"doctor", "--json"}, &doctor, inspect); err == nil || !bytes.Contains(doctor.Bytes(), []byte("needs-review")) {
		t.Fatalf("missing capture diagnostic: %s %v", doctor.String(), err)
	}
	expect := domain.CaptureAttempt(*p).Fingerprint()
	var output bytes.Buffer
	if err := RunCaptureRecovery(ctx, c, cwd, repo, []string{"list", "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("needs-review")) {
		t.Fatal(output.String())
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.CaptureRecovery.Resolve(ctx, repo, p.Proof.ID, expect, "No surviving original output", true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	doctor.Reset()
	if err := RunDiagnostics(ctx, cwd, []string{"doctor", "--json"}, &doctor, inspect); err != nil || !bytes.Contains(doctor.Bytes(), []byte("acknowledged-gap")) {
		t.Fatalf("acknowledged gap hidden or retried: %s %v", doctor.String(), err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("original capture bytes changed")
	}
	if err := replayPublications(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	assertNoPublication(t, c, repo)
	resolutionPath := filepath.Join(cwd, ".cxt", "worktrees", p.Proof.WorktreeID, "capture-resolutions", p.Proof.ID+".json")
	info, err := os.Stat(resolutionPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private resolution permissions: %v %v", info, err)
	}
	if _, err := c.CaptureRecovery.Resolve(ctx, repo, p.Proof.ID, expect, "changed reason", true); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("conflicting decision: %v", err)
	}
	// A corrupt resolution must never suppress future recovery diagnostics.
	if err := os.WriteFile(resolutionPath, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replayPublications(ctx, c, cwd); err == nil {
		t.Fatal("replay accepted corrupt resolution")
	}
}

func TestCaptureRecoveryJournalRejectsStaleOrInvalidWrite(t *testing.T) {
	cwd, c, _, repo, _ := publicationFixture(t)
	ctx := context.Background()
	p, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	witness := domain.CaptureAttempt(*p)
	r := domain.CaptureResolution{Version: 1, RepoID: repo, WorktreeID: p.Proof.WorktreeID, AttemptID: p.Proof.ID, AttemptHash: witness.Fingerprint(), Kind: "acknowledged-gap", Reason: "No recorded output", CreatedAt: time.Now().UTC()}
	j := capturejournal.New(cwd, cwd)
	bad := r
	bad.WorktreeID = "../escape"
	if err := j.WriteCaptureResolution(ctx, bad, []domain.CaptureAttempt{witness}); err == nil {
		t.Fatal("accepted malformed sidecar")
	}
	if got, err := j.ReadCaptureResolution(ctx, witness); err != nil || got != nil {
		t.Fatalf("invalid write changed disk: %v %v", got, err)
	}
	if err := p.recordOutcome(ctx, cwd, 0, "", "absent", inbound.SaveOutput{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := j.WriteCaptureResolution(ctx, r, []domain.CaptureAttempt{witness}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale review accepted: %v", err)
	}
	if got, err := j.ReadCaptureResolution(ctx, witness); err != nil || got != nil {
		t.Fatalf("stale write changed disk: %v %v", got, err)
	}
}

func TestCaptureRecoveryJournalReadDoesNotInitializeAndRejectsSymlinks(t *testing.T) {
	cwd := t.TempDir()
	repo := string(domain.HashContent([]byte("repo")))
	j := capturejournal.New(cwd, cwd)
	if got, err := j.ListCaptureAttempts(context.Background(), repo); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("read initialized a replica")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cwd, ".cxt")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ListCaptureAttempts(context.Background(), repo); err == nil {
		t.Fatal("followed symlink")
	}
}

func TestCaptureRecoveryRetryUsesFrozenOutputsOnly(t *testing.T) {
	cwd, c, st, repo, base := publicationFixture(t)
	ctx := context.Background()
	p, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.recordOutcome(ctx, cwd, 0, "/archived/session", "saved", inbound.SaveOutput{SnapshotID: base}, nil); err != nil {
		t.Fatal(err)
	}
	c.CaptureRecovery = app.NewCaptureRecoveryService(capturejournal.New(cwd, cwd), st)
	expected := domain.CaptureAttempt(*p).Fingerprint()
	var out bytes.Buffer
	if err := RunCaptureRecovery(ctx, c, cwd, repo, []string{"retry", p.Proof.ID, "--expect", string(expected)}, &out); err != nil {
		t.Fatal(err)
	}
	states, err := c.CaptureRecovery.Inspect(ctx, repo)
	if err != nil || len(states) != 1 || states[0].State != "completed" {
		t.Fatalf("%+v %v", states, err)
	}
	if err := RunCaptureRecovery(ctx, c, cwd, repo, []string{"retry", p.Proof.ID, "--expect", string(expected)}, &out); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale retry: %v", err)
	}
	if len(capturePasses(t, cwd)) != 1 {
		t.Fatal("retry began a new capture")
	}
}

func TestCaptureRecoveryCommandPreflight(t *testing.T) {
	for _, args := range [][]string{{"capture"}, {"capture", "show"}, {"capture", "resolve", "id"}, {"capture", "acknowledge", "id", "--expect", "hash"}, {"capture", "retry", "id", "--expect", "hash", "--reason", "no"}, {"capture", "list", "--expect", "hash"}, {"capture", "unknown"}} {
		if _, err := PreflightArgs(append([]string{"cxt"}, args...)); err == nil {
			t.Fatalf("accepted invalid command: %v", args)
		}
	}
}
