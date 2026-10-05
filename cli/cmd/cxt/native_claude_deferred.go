package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Preparation may run while an old TUI remains active. It only starts an idle
// native transport. Reading the first question, interaction, and TUI handoff
// happen in the owned Start callback, after the supervisor retires the old TUI.
func prepareNativeClaudeDeferred(ctx context.Context, cfg config, req delivcli.ProviderLaunchRequest) (result delivcli.DeferredProviderLaunch, resultErr error) {
	defer func() { resultErr = redactNativeClaudeContext(resultErr) }()
	if !req.Intent.Pull || !domain.ValidSessionID(req.NativeSessionID()) {
		return result, domain.ErrDeliveryFailed
	}
	bound, err := bindNativeClaudeLaunch(req)
	if err != nil {
		return result, err
	}
	preparer, _ := runtimeAgentLoader(cfg)
	checkPosition, err := pinNativeWorkingPosition(ctx, preparer, req.Cwd)
	if err != nil {
		return result, err
	}
	bound.options.Env = req.Environment(ctx)
	bound.options.SessionID = req.NativeSessionID()
	life, cancel := context.WithCancel(ctx)
	e, err := nativeclaude.StartFirstExchange(life, bound.options)
	if err != nil {
		cancel()
		return result, err
	}
	var once sync.Once
	var closeErr error
	cleanup := func() error {
		once.Do(func() { closeErr = e.Close(); cancel() })
		return redactNativeClaudeContext(nativeClaudeRetirementError(closeErr))
	}
	ready := false
	defer func() {
		if !ready {
			resultErr = errors.Join(resultErr, cleanup())
		}
	}()
	if e.SessionID() != req.NativeSessionID() {
		return result, domain.ErrHashMismatch
	}
	reader, err := newNativeClaudeContextReader(ctx, e)
	if err != nil {
		return result, err
	}
	args, err := e.ResumeArguments()
	if err != nil {
		return result, err
	}
	validate := func(ctx context.Context) error {
		if err := checkPosition(ctx); err != nil {
			return err
		}
		_, err := reader.AgentCapability(ctx, domain.ProviderClaude, reader.baseline.Model)
		return err
	}
	var attempted atomic.Bool
	start := func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, terminalStarted func()) (cmd *exec.Cmd, resultErr error) {
		defer func() { resultErr = redactNativeClaudeContext(resultErr) }()
		var err error
		if !attempted.CompareAndSwap(false, true) {
			return nil, domain.ErrDeliveryFailed
		}
		file, ok := stdin.(*os.File)
		if !ok || file == nil || stdout == nil || stderr == nil || terminalStarted == nil {
			return nil, domain.ErrDeliveryFailed
		}
		// All first-exchange operations, including user dialogs, have a finite
		// owner deadline. No timeout retries or replay of a released question.
		first, cancelFirst := context.WithTimeout(ctx, time.Hour)
		defer cancelFirst()
		console := newNativeClaudeConsole(func(ctx context.Context) (string, error) { return nativeConsoleLine(ctx, file) }, stderr)
		question := bound.prompt
		if !question.Present() {
			question, err = console.question(first)
			if err != nil {
				return nil, err
			}
		}
		if err := validate(first); err != nil {
			return nil, err
		}
		runner := nativeClaudeOrdinaryRunner{FirstExchange: e, handlers: console.handlers()}
		input, err := prepareNativeClaudeInput(first, cfg, req, e, runner, question)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintln(stderr, "cxt: latest main context prepared; Claude input uses conservative estimates and reserved headroom"); err != nil {
			return nil, err
		}
		answer, err := input.run(first)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintln(stdout, nativeConsoleText(answer.Answer)); err != nil {
			return nil, err
		}
		if answer.ReceiptError != nil {
			fmt.Fprintln(stderr, "cxt: the response completed, but its receipt could not be saved; the request will not be replayed")
		}
		path := e.OwnedArchivePath()
		if _, err := e.VerifyArchive(first, path); err != nil {
			return nil, err
		}
		plan, err := e.PrepareIdleResume(first, path)
		if err != nil {
			return nil, err
		}
		// A completed first turn can legitimately change code. Revalidate its
		// exact archive and launch here, not its pre-question code position.
		cmd, err = plan.StartSupervised(ctx, stdin, stdout, stderr)
		if err == nil {
			terminalStarted()
		}
		return cmd, err
	}
	ready = true
	return delivcli.DeferredProviderLaunch{Args: args, Env: req.Environment(ctx), SessionID: e.SessionID(),
		Validate: validate, Start: start, Cleanup: cleanup, Activate: validate}, nil
}

// Preserve normal text/newlines without allowing a native response to execute
// terminal control sequences during the CXT-owned first exchange.
func nativeConsoleText(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// Cancellation of an owned first exchange is still returned by Start. During
// retirement, a bare cancellation is not a failed process cleanup. Preserve
// every protocol/interaction/OS failure, including errors joined with it.
func nativeClaudeRetirementError(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, cause := range joined.Unwrap() {
			if kept := nativeClaudeRetirementError(cause); kept != nil {
				remaining = append(remaining, kept)
			}
		}
		return errors.Join(remaining...)
	}
	return err
}
