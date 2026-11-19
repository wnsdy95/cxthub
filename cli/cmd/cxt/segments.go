package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/sessionrecords"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// newSegmentReporter wires the read-only readers: Git, the Claude Code session
// files, and the Codex thread databases beside its sessions directory. With a
// cache directory the readers keep what they made of unchanged sources there;
// without one they write nothing anywhere.
func newSegmentReporter(cache string) (*app.SegmentReportService, error) {
	projects, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return nil, err
	}
	sessions, err := providerfs.CodexSessionsDir()
	if err != nil {
		return nil, err
	}
	claude := sessionrecords.NewClaudeRecords(projects)
	codex := sessionrecords.NewCodexRecords(filepath.Dir(sessions), gitctx.NormalizeRemoteURL)
	if cache != "" {
		claude, codex = claude.WithCache(cache), codex.WithCache(cache)
	}
	return app.NewSegmentReportService(gitctx.NewSessionGitRecords(), claude, codex), nil
}

// newSegmentUploader adds the repository's server to the same readers. It
// builds no local store.
func newSegmentUploader(cfg config, cache string) (*app.SegmentUploadService, error) {
	reports, err := newSegmentReporter(cache)
	if err != nil {
		return nil, err
	}
	remote := backendclient.NewResolvedBackendClient(func() (string, string, error) { return configuredTransport(cfg) }, cfg.Identity)
	// The label tells the uploader's own computers apart, like a device login's.
	machine, _ := os.Hostname()
	service := app.NewSegmentUploadService(reports, remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter()).CurrentRepo, machine)
	// Content is masked with the repository's policy and sent in the document
	// identity the repository captures with, both exactly as capture does.
	injectScrubOptions()
	content := backendclient.NewSessionContentUploader(remote, func(ctx context.Context) (domain.DocumentIdentity, error) {
		observed, err := remotecfg.Observe(ctx, cfg.RepoRoot)
		if err != nil {
			return "", err
		}
		return observed.CaptureIdentity()
	})
	service.WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{
		domain.ProviderClaude: sessionrecords.NewClaudeContent(),
	})
	// Branch IDs live in Git's own branch configuration. This is the one
	// thing an upload writes on this computer.
	// The server is told the names of this computer's worktrees, never their
	// paths, so that a worktree no session worked in is still listed.
	return service.WithBranchIDs(gitctx.NewBranchIDs(), remote, gitctx.NewBranchID).WithWorktreeList(remote), nil
}

// newSegmentCollector is the same upload for the detached collector a hook
// wakes. It adds the memory of what was sent, kept beside Git's own files.
func newSegmentCollector(cfg config) (inbound.SegmentCollector, *gitctx.CollectorFiles, error) {
	// A run nobody waits for reads again only what changed. The cache is the
	// user's own, outside every repository and every tool's records.
	cache := ""
	if dir, err := os.UserCacheDir(); err == nil {
		cache = filepath.Join(dir, "cxthub", "session-records-v1")
	}
	service, err := newSegmentUploader(cfg, cache)
	if err != nil {
		return nil, nil, err
	}
	files := gitctx.NewCollectorFiles()
	// Content is rebuilt when the masking policy or the document identity changes.
	epoch := func(ctx context.Context, _ string) string {
		policy, _ := capture.ScrubPolicyFingerprint(cfg.RepoRoot)
		identity := ""
		if observed, err := remotecfg.Observe(ctx, cfg.RepoRoot); err == nil {
			if captured, err := observed.CaptureIdentity(); err == nil {
				identity = string(captured)
			}
		}
		return policy + "\n" + identity
	}
	return service.WithCollectorState(files, time.Now, epoch), files, nil
}
