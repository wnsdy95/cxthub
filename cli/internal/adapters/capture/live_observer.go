package capture

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func ObserveLiveSession(ctx context.Context, cwd string, provider domain.ProviderKind, id string, wake func(string)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	return WatchSession(ctx, cwd, provider, id, liveSessionWatch(exe, cwd, provider, id, wake))
}

func liveSessionWatch(exe, cwd string, provider domain.ProviderKind, id string, wake func(string)) SessionWatch {
	run := func(phase string) func(context.Context) error {
		return func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, exe, "git-hook", phase, string(provider), id)
			command.Dir = cwd
			return command.Run()
		}
	}
	return SessionWatch{
		Poll: run("live-capture"), Publish: run("live-publish"),
		Wake: func(context.Context) {
			if wake != nil {
				wake(cwd)
			}
		},
	}
}
