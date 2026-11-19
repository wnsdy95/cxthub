package main

import (
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/sessionrecords"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
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
	return app.NewSegmentUploadService(reports, remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter()).CurrentRepo, machine), nil
}
