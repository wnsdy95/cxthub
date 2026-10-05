package cli

import (
	"context"
	"errors"
	"os/exec"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type providerStartResult struct {
	cmd *exec.Cmd
	err error
}

// The supervisor alone adopts and waits for a child, including one returned
// after cancellation. A starter must finish before its runtime can be cleaned.
type providerLaunchProcess struct {
	child    *exec.Cmd
	done     <-chan error
	starting <-chan providerStartResult
	cancel   context.CancelFunc
	startErr error
}

func (p *providerLaunchProcess) start(ctx context.Context, run func(context.Context) (*exec.Cmd, error)) {
	ctx, p.cancel = context.WithCancel(ctx)
	result := make(chan providerStartResult, 1)
	p.starting = result
	go func() {
		cmd, err := run(ctx)
		result <- providerStartResult{cmd, err}
	}()
}

func (p *providerLaunchProcess) adopt(cmd *exec.Cmd) {
	p.child = cmd
	done := make(chan error, 1)
	p.done = done
	go func() { done <- cmd.Wait() }()
}

func (p *providerLaunchProcess) started(result providerStartResult) error {
	p.starting = nil
	p.startErr = result.err
	if result.cmd != nil && result.cmd.Process != nil {
		p.adopt(result.cmd)
	} else if p.startErr == nil {
		p.startErr = domain.ErrDeliveryFailed
	}
	return p.startErr
}

func (p *providerLaunchProcess) stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.starting != nil {
		p.started(<-p.starting)
		// Only cancellation caused by retiring this starter is normal. In
		// particular, a joined protocol/cleanup error must block replacement.
		if providerStartOnlyCanceled(p.startErr) {
			p.startErr = nil
		}
	}
	var exitErr error
	if p.done != nil {
		exitErr = stopProviderChild(p.child, p.done)
		p.done = nil
	}
	return errors.Join(p.startErr, exitErr)
}

func providerStartOnlyCanceled(err error) bool {
	if err == context.Canceled {
		return true
	}
	switch err := err.(type) {
	case interface{ Unwrap() []error }:
		causes := err.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !providerStartOnlyCanceled(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return providerStartOnlyCanceled(err.Unwrap())
	}
	return false
}
