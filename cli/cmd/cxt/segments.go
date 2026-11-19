package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
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
// files, and the Codex thread databases beside its sessions directory.
func newSegmentReporter() (*app.SegmentReportService, error) {
	claude, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return nil, err
	}
	codex, err := providerfs.CodexSessionsDir()
	if err != nil {
		return nil, err
	}
	return app.NewSegmentReportService(
		gitctx.NewSessionGitRecords(),
		sessionrecords.NewClaudeRecords(claude),
		sessionrecords.NewCodexRecords(filepath.Dir(codex), gitctx.NormalizeRemoteURL),
	), nil
}

// newSegmentUploader adds the repository's server to the same readers. It
// builds no local store: an upload writes nothing on this computer.
func newSegmentUploader(cfg config) (inbound.SegmentUploader, error) {
	reports, err := newSegmentReporter()
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
	return service.WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{
		domain.ProviderClaude: sessionrecords.NewClaudeContent(),
	}), nil
}
