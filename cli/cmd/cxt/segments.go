package main

import (
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/sessionrecords"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// newSegmentReporter wires the read-only readers: Git, the Claude Code session
// files, and the Codex thread databases beside its sessions directory.
func newSegmentReporter() (inbound.SegmentReporter, error) {
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
