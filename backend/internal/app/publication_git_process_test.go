package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPublicationGitScanRecoversAfterProviderExit(t *testing.T) {
	if directory := os.Getenv("CXT_PUBLICATION_PROVIDER_PROCESS"); directory != "" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer cancel()
		storage := store.NewFSStore(directory)
		core := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
		scans, err := NewGitScans(core, gitevidence.NewGitHub(func() string { return "" }))
		if err != nil {
			t.Fatal(err)
		}
		if err := scans.Process(ctx, 1); err != nil {
			t.Fatal(err)
		}
		fmt.Println("READY")
		<-ctx.Done()
		return
	}
	reader, commits := newPublicationGitFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, "data")
	storage := store.NewFSStore(directory)
	core := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: publicationGitOrigin, RemoteURL: "http://127.0.0.1:19999/acme/publication"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(reader.directory, filepath.Join(root, "publication-host")); err != nil {
		t.Fatal(err)
	}
	job := domain.NewGitScan(repo, publicationGitOrigin, commits[3], time.Now().UTC())
	if err := storage.EnqueueGitScan(ctx, job); err != nil {
		t.Fatal(err)
	}
	provider := exec.CommandContext(ctx, executable, "-test.run=^TestPublicationGitScanRecoversAfterProviderExit$")
	provider.Env = append(os.Environ(), "CXT_PUBLICATION_PROVIDER_PROCESS="+directory)
	var diagnostics bytes.Buffer
	provider.Stderr = &diagnostics
	output, err := provider.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if provider.ProcessState == nil {
			provider.Process.Kill()
			provider.Wait()
		}
	})
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "READY\n" {
		t.Fatalf("provider readiness: %q %v", ready, err)
	}
	if err := provider.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := provider.Wait(); err != nil {
		t.Fatalf("provider did not exit cleanly: %v: %s", err, diagnostics.String())
	}
	failed, err := storage.GetGitScan(ctx, repo, job.ID)
	if err != nil || failed.State != "attention" || failed.Reason != "invalid_or_ambiguous_git_evidence" {
		t.Fatalf("provider did not leave the expected failure: %+v %v", failed, err)
	}
	fixture := exec.CommandContext(ctx, executable, "-test.run=^TestPublicationE2EGitEvidence$", "-test.v")
	fixture.Env = append(os.Environ(), "CXT_E2E_GIT_FIXTURE_ROOT="+root, "CXT_E2E_GIT_FIXTURE_REPO="+string(repo), "CXT_E2E_GIT_FIXTURE_MERGE="+commits[3], "CXT_E2E_DSN=")
	if output, err := fixture.CombinedOutput(); err != nil {
		t.Fatalf("exclusive fixture process: %v: %s", err, output)
	}
	jobs, err := storage.ListGitScans(ctx, repo, "", 100)
	if err != nil || len(jobs) != len(commits) {
		t.Fatalf("incomplete fixture closure: %+v %v", jobs, err)
	}
	for _, job := range jobs {
		if job.State != "completed" || !job.TreeIndexed || !job.Indexed {
			t.Fatalf("unfinished fixture job: %+v", job)
		}
	}
	refs, err := core.ListRefs(ctx, repo)
	if err != nil || len(refs) != 0 {
		t.Fatalf("fixture changed context refs: %+v %v", refs, err)
	}
	history, err := core.ListHistory(ctx, repo)
	if err != nil || len(history) != 0 {
		t.Fatalf("fixture changed context history: %+v %v", history, err)
	}
}
