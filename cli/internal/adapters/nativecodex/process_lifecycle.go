package nativecodex

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
)

// processExitObserver observes a child exiting WITHOUT reaping it. Until the
// sole cmd.Wait below runs, that child keeps its PID reserved, including on
// platforms where Go cannot provide a generation-bound process handle.
type processExitObserver interface {
	wait(pid int) error
	close()
}

type ownedProcessState struct {
	mu             sync.Mutex
	retired        bool // irreversible: no further signals or peer checks
	observationErr error
}

func startOwnedProcess(cmd *exec.Cmd, dir string) (ownedProcess, error) {
	observer, err := newProcessExitObserver()
	if err != nil {
		return ownedProcess{}, err
	}
	return startObservedProcess(cmd, dir, observer)
}

func startObservedProcess(cmd *exec.Cmd, dir string, observer processExitObserver) (ownedProcess, error) {
	started := false
	defer func() {
		if !started {
			observer.close()
		}
	}()
	// A real file avoids os/exec pipe-copy goroutines, even when a descendant
	// inherits stdout/stderr. WaitDelay is disabled because watchCtx can call
	// Process.Kill directly, bypassing our lifecycle lock.
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return ownedProcess{}, err
	}
	defer null.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.WaitDelay = 0
	p := ownedProcess{cmd: cmd, done: make(chan struct{}), dir: dir, state: &ownedProcessState{}}
	cmd.Cancel = func() error { return p.signal(true) }
	if err := cmd.Start(); err != nil {
		return ownedProcess{}, err
	}
	started = true
	go p.observeAndWait(observer)
	return p, nil
}

func (p ownedProcess) observeAndWait(observer processExitObserver) {
	err := observer.wait(p.cmd.Process.Pid) // may block; never holds state.mu
	observer.close()

	p.state.mu.Lock()
	p.state.retired = true
	if err != nil {
		p.state.observationErr = fmt.Errorf("%w: owned process exit observation failed", ErrProtocol)
		// Observation failure is terminal. Kill our still-unreaped child before
		// permitting Wait. ErrProcessDone means ownership was already lost;
		// never signal that numeric PID in that case.
		if !errors.Is(err, os.ErrProcessDone) {
			_ = signalOwnedProcess(p.cmd, true)
		}
	}
	p.state.mu.Unlock()

	// Retiring under the same lock as signal/ownsPeer is the reaping barrier:
	// all earlier operations have finished and all later ones are rejected.
	// Do NOT hold the mutex through Cmd.Wait: it joins watchCtx, which may be
	// in Cancel and need this lock. No os/exec kill timer or pipe pump exists.
	_ = p.cmd.Wait()
	close(p.done)
}

func (p ownedProcess) signal(force bool) error {
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if p.state.retired {
		return os.ErrProcessDone
	}
	return signalOwnedProcess(p.cmd, force)
}

func (p ownedProcess) ownsPeer(conn net.Conn) bool {
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if p.state.retired {
		return false
	}
	peer, err := peerProcess(conn)
	return err == nil && peer == p.cmd.Process.Pid && ownedProcessAlive(p.cmd)
}
