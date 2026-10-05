package cli

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// The wrapper and native TUI share a foreground process group. Keep receiving
// SIGINT in the wrapper while the TUI runs, but let the TUI handle its own copy.
// SIGTERM always retires the whole owned lifecycle through the supervisor.
type providerLaunchSignals struct {
	native  atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	signals chan os.Signal
	stop    chan struct{}
	done    chan struct{}
}

func ownProviderLaunchSignals(ctx context.Context) (context.Context, *providerLaunchSignals) {
	ctx, cancel := context.WithCancel(ctx)
	s := &providerLaunchSignals{ctx: ctx, cancel: cancel, signals: make(chan os.Signal, 8), stop: make(chan struct{}), done: make(chan struct{})}
	signal.Notify(s.signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(s.done)
		for {
			select {
			case sig := <-s.signals:
				s.receive(sig)
			case <-s.stop:
				return
			}
		}
	}()
	return ctx, s
}

func (s *providerLaunchSignals) receive(sig os.Signal) {
	if sig == syscall.SIGTERM || !s.native.Load() {
		s.cancel()
	}
}

func (s *providerLaunchSignals) terminalStarted() {
	if s != nil {
		// A handoff changes future signal handling; it never clears an already
		// canceled context. The supervisor still joins and reaps a late child.
		s.native.Store(true)
	}
}

func (s *providerLaunchSignals) ownedInput() {
	if s != nil {
		s.native.Store(false)
	}
}

func (s *providerLaunchSignals) close() error {
	defer s.cancel()
	signal.Stop(s.signals)
	// Stop waits for signal delivery to quiesce. Settle the finite buffer
	// before stopping the receiver, then join any receive already in flight.
drain:
	for {
		select {
		case sig := <-s.signals:
			s.receive(sig)
		default:
			break drain
		}
	}
	close(s.stop)
	<-s.done
	// Ordinary resource cleanup must not turn a successful exit into a cancel.
	return s.ctx.Err()
}
