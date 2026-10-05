package nativeclaude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type processExitObserver interface {
	wait(int) error
	close()
}

type process struct {
	cmd                      *exec.Cmd
	stdin, stdout, stderr    *os.File
	exited, outDone, errDone chan struct{}
	abortRequested           chan struct{}
	observationErr           error
	onError                  func(error)
	closeOnce                sync.Once
	closeErr                 error
	retirementErr            error
}

// OS exit observation leaves the child unreaped. Close is the sole signaler
// and reaper, so its process-group ID cannot be reused during cleanup.
func startProcess(executable string, args []string, cwd string, env []string, frame func([]byte) error, onError func(error)) (*process, error) {
	observer, err := newProcessExitObserver()
	if err != nil {
		return nil, ErrState
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = cwd
	cmd.Env = env
	configureProcess(cmd)
	var files []*os.File
	pipe := func() (*os.File, *os.File, error) {
		r, w, e := os.Pipe()
		if e == nil {
			files = append(files, r, w)
		}
		return r, w, e
	}
	started := false
	defer func() {
		if !started {
			observer.close()
			for _, f := range files {
				_ = f.Close()
			}
		}
	}()
	inR, inW, err := pipe()
	if err != nil {
		return nil, ErrState
	}
	outR, outW, err := pipe()
	if err != nil {
		return nil, ErrState
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, ErrState
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err = cmd.Start(); err != nil {
		return nil, ErrState
	}
	started = true
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()
	p := &process{cmd: cmd, stdin: inW, stdout: outR, stderr: errR, exited: make(chan struct{}), outDone: make(chan struct{}), errDone: make(chan struct{}), abortRequested: make(chan struct{}, 1), onError: onError}
	go func() { p.observationErr = observer.wait(cmd.Process.Pid); observer.close(); close(p.exited) }()
	go func() {
		defer close(p.outDone)
		defer outR.Close()
		if err := readFrames(outR, frame); err != nil {
			onError(err)
		}
	}()
	go func() {
		defer close(p.errDone)
		defer errR.Close()
		// Discard diagnostics without storing or rendering paths, config, account
		// data or prompt bodies. A flooding process invalidates the session.
		n, err := io.CopyN(io.Discard, errR, 1<<20)
		if n == 1<<20 {
			onError(ErrLimit)
		} else if err != nil && !errors.Is(err, io.EOF) {
			onError(ErrProtocol)
		}
	}()
	return p, nil
}

func readFrames(r io.Reader, frame func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) > 0 && !bytes.ContainsRune(data, '\n') {
			return 0, nil, ErrProtocol
		}
		return bufio.ScanLines(data, atEOF)
	})
	var total int64
	for scanner.Scan() {
		raw := scanner.Bytes()
		total += int64(len(raw) + 1)
		if total > 96<<20 {
			return ErrLimit
		}
		if len(raw) == 0 {
			return ErrProtocol
		}
		if err := frame(raw); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		if errors.Is(scanner.Err(), ErrProtocol) {
			return ErrProtocol
		}
		return ErrLimit
	}
	return nil
}

func (p *process) write(ctx context.Context, raw []byte) error {
	if len(raw) > maxFrameBytes {
		return ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Abort stops the owned group before closing its input to interrupt a write.
	// Cancellation is irreversible: a partial frame may have reached native.
	stop := context.AfterFunc(ctx, func() {
		_ = p.shutdown(true)
	})
	defer stop()
	n, err := p.stdin.Write(raw)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || n != len(raw) {
		return ErrClosed
	}
	return nil
}

func waitFor(ch <-chan struct{}, duration time.Duration) bool {
	t := time.NewTimer(duration)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

func (p *process) close() error { return p.shutdown(false) }

// Request escalation without signaling or reaping. Only the shutdown owner
// consumes it, before Wait; requests after retirement cannot signal a reused PID.
// A nil channel permits inert process fixtures without starting a new owner.
func (p *process) requestAbort() {
	select {
	case p.abortRequested <- struct{}{}:
	default:
	}
}

func (p *process) shutdown(abort bool) error {
	if abort {
		p.requestAbort()
	}
	p.closeOnce.Do(func() {
		// An abort requester can lose the Once race to a normal closer. Honor
		// the queued request before that closer sends EOF.
		select {
		case <-p.abortRequested:
			abort = true
		default:
		}
		forced := false
		closeInput := !abort
		retirementFailed := func() { p.closeErr, p.retirementErr = ErrCleanup, ErrCleanup }
		if abort {
			// EOF during a permission dialog can mean "deny and continue" to
			// native. An abandoned exchange must stop the owned group before
			// closing its input, rather than inadvertently releasing another turn.
			forced = true
			if err := cleanupProcessGroup(p.cmd); err != nil {
				retirementFailed()
			}
			if !waitFor(p.exited, time.Second) || p.observationErr != nil {
				retirementFailed()
			} else {
				closeInput = true
			}
		}
		if closeInput {
			_ = p.stdin.Close()
		} else {
			// A denied signal or failed observer is not proof of exit. Interrupt
			// our writer without sending an EOF that could continue native work.
			_ = p.stdin.SetWriteDeadline(time.Now())
		}
		// Normal EOF runs native session teardown and registered cleanup. The
		// pinned host has two-second cleanup/refresh bounds; a one-second wait
		// can kill a successful session before its write queues finish. Keep a
		// bounded grace only for this path. Abort still signals before EOF and
		// retains its short retirement checks; forced exits remain failures.
		exited := false
		if abort {
			exited = waitFor(p.exited, time.Second)
		} else {
			grace := time.NewTimer(5 * time.Second)
			select {
			case <-p.exited:
				exited = true
			case <-p.abortRequested:
				// EOF has already been sent. Escalate here, in the same owner,
				// rather than blocking another abort behind closeOnce.
				forced = true
				if err := cleanupProcessGroup(p.cmd); err != nil {
					retirementFailed()
				}
				exited = waitFor(p.exited, time.Second)
			case <-grace.C:
			}
			grace.Stop()
		}
		if !exited {
			forced = true
			_ = signalProcessGroup(p.cmd, false)
			if !waitFor(p.exited, 500*time.Millisecond) {
				_ = signalProcessGroup(p.cmd, true)
			}
		}
		observed := waitFor(p.exited, time.Second)
		// The unreaped child still reserves the group identity. Dispose any
		// descendants holding inherited descriptors before allowing Wait.
		if err := cleanupProcessGroup(p.cmd); err != nil {
			retirementFailed()
		}
		reaped := make(chan struct{})
		var waitErr error
		go func() {
			waitErr = p.cmd.Wait()
			if p.cmd.ProcessState != nil {
				// Even after our bounded wait expires, actual leader exit must
				// eventually release the retained input descriptor.
				_ = p.stdin.Close()
			}
			close(reaped)
		}()
		if !waitFor(reaped, time.Second) {
			retirementFailed()
		} else {
			if waitErr != nil {
				p.closeErr = ErrCleanup
				var exited *exec.ExitError
				if !errors.As(waitErr, &exited) {
					retirementFailed()
				}
			}
		}
		if forced {
			p.closeErr = ErrCleanup
		}
		if !observed || p.observationErr != nil {
			retirementFailed()
		}
		if !waitFor(p.outDone, time.Second) {
			_ = p.stdout.Close()
			retirementFailed()
		}
		if !waitFor(p.errDone, time.Second) {
			_ = p.stderr.Close()
			retirementFailed()
		}
		// Pipe closure must also join the readers before a persistence claim.
		if !waitFor(p.outDone, time.Second) || !waitFor(p.errDone, time.Second) {
			retirementFailed()
		}
	})
	return p.closeErr
}

func readVersionFor(ctx context.Context, exe, cwd string, env []string, expected string) (string, error) {
	if expected != supportedVersion {
		return "", ErrState
	}
	var mu sync.Mutex
	var version string
	var protocolErr error
	p, err := startProcess(exe, []string{"--version"}, cwd, env, func(raw []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if version != "" || !bytes.Equal(bytes.TrimSpace(raw), raw) {
			return ErrProtocol
		}
		if string(raw) != expected+" (Claude Code)" {
			return ErrProtocol
		}
		version = expected
		return nil
	}, func(err error) { mu.Lock(); protocolErr = err; mu.Unlock() })
	if err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		_ = p.close()
		return "", ctx.Err()
	case <-p.exited:
	case <-p.outDone:
	}
	err = p.close()
	mu.Lock()
	defer mu.Unlock()
	if err != nil || protocolErr != nil || strings.TrimSpace(version) == "" {
		return "", errors.Join(ErrProtocol, err)
	}
	return version, nil
}
