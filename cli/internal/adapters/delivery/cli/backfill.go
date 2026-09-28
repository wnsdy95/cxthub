package cli

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, err := storage.NewFileStore(root).WithBackfillDaemon(ctx, func() error {
		for {
			out, err := syncer.SyncHistorical(ctx, inbound.SyncInput{Cwd: cwd}, 8)
			if err != nil {
				return err
			}
			if out.Busy || out.Pending == 0 {
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
