package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func wakeHistoricalSync(c *Container, cwd string) {
	if c.WakeHistoricalSync != nil {
		c.WakeHistoricalSync(cwd)
	}
}

// SpawnHistoricalSync is wired only by the executable composition root.
func SpawnHistoricalSync(cwd string) {
	root := cxtRepoRoot(context.Background(), cwd)
	if _, ok := remotecfg.Origin(root); !ok && os.Getenv("CXT_REMOTE") == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "historical-sync")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// This helper never captures a provider or replays branch operations. Durable
// per-job retry state survives its bounded lifetime; the next push wakes it.
// The daemon lock prevents every foreground invocation from adding a sleeper.
func runHistoricalSync(ctx context.Context, c *Container, cwd string) error {
	syncer, ok := c.Sync.(inbound.HistoricalSync)
	if !ok {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	if _, ok := remotecfg.Origin(root); !ok && os.Getenv("CXT_REMOTE") == "" {
		return nil
	}
	if c.WithBackfillDaemon == nil {
		return fmt.Errorf("historical synchronization lock unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, err := c.WithBackfillDaemon(ctx, root, func() error {
		for {
			out, err := syncer.SyncHistorical(ctx, inbound.SyncInput{Cwd: cwd}, 8)
			if err != nil {
				return err
			}
			if out.Busy || out.Paused || out.Pending == 0 {
				return nil
			}
			wait := time.Until(out.NextAttempt)
			if wait < time.Second {
				wait = time.Second
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
	return err
}

// RunHistoricalUploadsPause pauses or resumes background uploads of retained
// history. Queue entries, and so retention pins, are untouched; an explicit
// `cxt push --wait-history` still uploads everything.
func RunHistoricalUploadsPause(ctx context.Context, c *Container, cwd string, pause bool, w io.Writer) error {
	if c == nil || c.SetHistoryPaused == nil || c.Inspection == nil {
		return fmt.Errorf("historical upload control unavailable")
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !state.Initialized {
		return fmt.Errorf("no initialized local replica; use cxt doctor to inspect registration and damage")
	}
	repo, err := c.Inspection.ResolveRepo(ctx, state.Root, cwd)
	if err != nil {
		return err
	}
	control, err := c.SetHistoryPaused(ctx, state.Root, string(repo.ID), pause)
	if err != nil {
		return err
	}
	jobs, err := c.Inspection.ListBackfills(ctx, state.Root, string(repo.ID))
	if err != nil {
		return err
	}
	if pause {
		fmt.Fprintf(w, "Retained history uploads paused: %d snapshot(s) stay queued and retained on this machine. Resume with cxt sync resume-history.\n", len(jobs))
		return nil
	}
	fmt.Fprintf(w, "Retained history uploads resumed: %d snapshot(s) queued.\n", len(jobs))
	if control.HoldUntil.After(time.Now()) {
		fmt.Fprintf(w, "The next upload starts after %s.\n", control.HoldUntil.Format(time.RFC3339))
	}
	if len(jobs) > 0 {
		wakeHistoricalSync(c, cwd)
	}
	return nil
}
