package nativeclaude

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// OwnedArchivePath names only this invocation's native archive. Returning a
// pathname is not a persistence claim; VerifyArchive still checks its contents.
func (e *FirstExchange) OwnedArchivePath() string {
	if e == nil || e.s == nil {
		return ""
	}
	return filepath.Join(e.s.archiveRoot, providerfs.EncodeCwd(e.s.cwd), e.s.id+".jsonl")
}

// ResumeArguments permits the supervisor to freeze its expected invocation
// before the first question. Only a verified IdleResumePlan can start it.
func (e *FirstExchange) ResumeArguments() ([]string, error) {
	if e == nil || e.s == nil {
		return nil, ErrState
	}
	return e.s.launch.resumeArgs(e.OwnedArchivePath())
}

// StartSupervised hands a started native TUI to the existing CLI supervisor.
// The caller exclusively owns Wait and termination. It shares the controlling
// terminal's foreground group, just like the supervisor's ordinary child;
// unlike Start it never installs the isolated-PTY process-group lifecycle.
// The opaque plan's one-shot and final archive checks are shared with Start.
func (p *IdleResumePlan) StartSupervised(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (*exec.Cmd, error) {
	cmd, err := p.resumeCommand(ctx, stdin, stdout, stderr)
	if err != nil {
		return nil, err
	}
	if err := p.startResumeCommand(ctx, cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}
