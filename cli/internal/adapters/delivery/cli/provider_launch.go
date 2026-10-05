package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	memoryadapter "github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ProviderLaunchHooks connects the delivery adapter to the application package
// use case without importing app. Prepare must pin selection, validate source
// integrity, and materialize atomically. Record must durably persist receipts;
// a process start is never evidence that the provider accepted the package.
type ProviderLaunchHooks struct {
	Prepare func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error)
	// PrepareDeferred owns a native runtime for fresh native history launches.
	// When configured, failure never falls back to materialized delivery.
	PrepareDeferred func(context.Context, ProviderLaunchRequest) (DeferredProviderLaunch, error)
	Record          func(context.Context, ProviderLaunchReceipt) error
}

type ProviderLaunchRequest struct {
	Cwd            string
	Executable     string
	Intent         LaunchIntent
	Transition     *ProviderLaunchTransition
	environment    []string // frozen before deferred preparation; access through Environment
	ownedSessionID string   // assigned only by the supervisor before native Claude starts
}

// ResumeArguments constructs the only supported prepared CLI transport: a new,
// materialized session followed by the exact original provider arguments.
// It never quotes or reparses a shell command.
func (r ProviderLaunchRequest) ResumeArguments(sessionID string) ([]string, error) {
	if !providerfs.ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid materialized provider session ID")
	}
	if _, err := inspectLaunchIntent(r.Intent); err != nil {
		return nil, err
	}
	return append(resumeArgs(r.Intent.Provider, sessionID), r.Intent.ProviderArgs...), nil
}

type ProviderLaunchTransition struct {
	PreviousBranch string
	Branch         string
	BoundarySeedID string
}

type PreparedProviderLaunch struct {
	// Deferred is a lifecycle-only runtime, mutually exclusive with package proof.
	Deferred *DeferredProviderLaunch
	// Bootstrap proves an authorized empty repository, including a genuine
	// unborn Git branch when CodeCommit is empty. Validate must recheck it.
	Bootstrap         *domain.AgentBootstrapProof
	Budget            *domain.AgentContextBudget
	PromptReservation domain.AgentPromptReservation `json:"-"`
	Args              []string
	SessionID         string
	PackageHash       domain.ContentHash
	CodeCommit        string
	SourceRevision    string
	SelectedTokens    int
	TokenMeasurement  string
	// Capability is "verified_for_preparation" only after validating the selected model,
	// authentication path, host version, overhead, and compaction threshold.
	Capability string
	// Cleanup releases only temporary delivery resources. It must never remove
	// source records or a materialized persistent provider session.
	Cleanup func() error
	// Validate checks mutable selection/code immediately before process start.
	Validate func(context.Context) error
}

type ProviderLaunchReceipt struct {
	SessionID           string                           `json:"session_id,omitempty"`
	TurnID              string                           `json:"turn_id,omitempty"`
	Outcome             string                           `json:"outcome,omitempty"`
	Bootstrap           *domain.AgentBootstrapProof      `json:"bootstrap,omitempty"`
	Budget              *domain.AgentContextBudget       `json:"budget,omitempty"`
	NativeInputEstimate *domain.AgentNativeInputEstimate `json:"native_input_estimate,omitempty"`
	Version             int                              `json:"version"`
	Provider            domain.ProviderKind              `json:"provider"`
	PackageHash         domain.ContentHash               `json:"package_hash,omitempty"`
	Mode                string                           `json:"mode"`
	RequestedBudget     int                              `json:"requested_budget,omitempty"`
	SelectedTokens      int                              `json:"selected_tokens,omitempty"`
	TokenMeasurement    string                           `json:"token_measurement,omitempty"`
	CodeCommit          string                           `json:"code_commit,omitempty"`
	SourceRevision      string                           `json:"source_revision,omitempty"`
	Capability          string                           `json:"capability,omitempty"`
	State               string                           `json:"state"`
	Acceptance          string                           `json:"acceptance"`
	Failure             string                           `json:"failure,omitempty"`
}

// RunProviderLaunch launches CLI providers only. Desktop apps require their
// own handoff/MCP adapter; app commands are ordinary provider passthrough.
func RunProviderLaunch(ctx context.Context, cwd string, intent LaunchIntent, hooks ProviderLaunchHooks) error {
	return runProviderLaunch(ctx, cwd, intent, hooks, providerLaunchRuntime{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		interactive:   providerLaunchTerminal(os.Stdin) && providerLaunchTerminal(os.Stdout),
		pollInterval:  time.Second,
		handleSignals: true,
	})
}

type providerLaunchRuntime struct {
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	interactive   bool
	pollInterval  time.Duration
	handleSignals bool
}

type providerLaunchWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *providerLaunchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func runProviderLaunch(ctx context.Context, cwd string, intent LaunchIntent, hooks ProviderLaunchHooks, runtime providerLaunchRuntime) (resultErr error) {
	var signals *providerLaunchSignals
	if runtime.handleSignals {
		ctx, signals = ownProviderLaunchSignals(ctx)
		defer func() {
			resultErr = errors.Join(resultErr, signals.close())
		}()
	}
	// The supervisor can report a failed restart while the child still writes.
	// Keep native terminal descriptors intact; serialize custom embedding writers.
	if _, native := runtime.stderr.(*os.File); !native {
		runtime.stderr = &providerLaunchWriter{writer: runtime.stderr}
	}
	inv, err := inspectLaunchIntent(intent)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Own the original argv for the entire supervision lifetime. Neither
	// package preparation nor a caller may mutate the next restart's policy.
	intent.ProviderArgs = append([]string(nil), intent.ProviderArgs...)
	if intent.WorkStatePath != "" && !filepath.IsAbs(intent.WorkStatePath) {
		intent.WorkStatePath = filepath.Join(cwd, intent.WorkStatePath)
	}
	bin, err := exec.LookPath(string(intent.Provider))
	if err != nil {
		return fmt.Errorf("%s executable not found: %w", intent.Provider, err)
	}
	selectionCwd := cwd
	if inv.Directory != "" {
		selectionCwd = inv.Directory
		if !filepath.IsAbs(selectionCwd) {
			selectionCwd = filepath.Join(cwd, selectionCwd)
		}
	}
	if inv.Mode == providerFresh && !runtime.interactive {
		if intent.WorkStatePath != "" {
			return fmt.Errorf("prefix --work-state requires an interactive CLI terminal; no personal handoff was delivered")
		}
		if intent.Pull {
			return fmt.Errorf("prefix --pull requires an interactive CLI terminal; no context was delivered")
		}
		inv.Mode = providerNoninteractive
	}
	managed := inv.Mode == providerFresh
	supervised := managed || inv.Mode == providerNative
	deferred := managed && nativeDeferredProvider(intent.Provider) && intent.Pull && hooks.PrepareDeferred != nil
	if managed && ((!deferred && hooks.Prepare == nil) || hooks.Record == nil) {
		return fmt.Errorf("CXT context preparation is unavailable; %s was not started. Run %s directly to explicitly start without CXT context", intent.Provider, intent.Provider)
	}
	request := ProviderLaunchRequest{Cwd: selectionCwd, Executable: bin, Intent: intent}
	args := append([]string(nil), intent.ProviderArgs...)
	if runtime.pollInterval <= 0 {
		runtime.pollInterval = time.Second
	}
	type readyLaunch struct {
		prepared PreparedProviderLaunch
		receipt  ProviderLaunchReceipt
		observed time.Time
	}
	var next *readyLaunch
	defer func() {
		if next != nil {
			if err := cleanupProviderLaunch(next.prepared); err != nil {
				resultErr = errors.Join(resultErr, launchFailure(ctx, hooks, next.receipt, true, err))
			}
		}
	}()
	for {
		// Include transitions during preparation, receipt writes and index warming.
		start := time.Now()
		var prepared PreparedProviderLaunch
		var receipt ProviderLaunchReceipt
		cleanup := func() error { return cleanupProviderLaunch(prepared) }
		fail := func(cause error) error {
			return launchFailure(ctx, hooks, receipt, managed, errors.Join(cause, cleanup()))
		}
		if managed {
			prewarmed := next != nil
			if next != nil {
				prepared, receipt, start = next.prepared, next.receipt, next.observed
				next = nil
			} else {
				prepared, receipt, err = prepareProviderLaunch(ctx, request, hooks)
				if err != nil {
					return err
				}
			}
			args = append([]string(nil), prepared.Args...)
			if prepared.Deferred != nil {
				args = append([]string(nil), prepared.Deferred.Args...)
			}
			// Preserve native session index registration for newly materialized
			// Codex sessions. This is best effort and never counts as acceptance.
			if !prewarmed && prepared.Deferred == nil {
				warmAgentIndexContext(ctx, bin, intent.Provider, selectionCwd, prepared.SessionID)
			}
			if err = validateReadyLaunch(ctx, request, start, prepared); err != nil {
				return fail(err)
			}
			limits := ""
			if b := receipt.Budget; b != nil {
				limits = fmt.Sprintf(" model=%q effective_budget=%d initial_input_limit=%d host_input_tokens=%d framing_tokens=%d initial_prompt_tokens=%d reserved_tokens=%d context_window=%d", b.Model, b.EffectiveTokens, b.InitialInputLimit, b.HostInputTokens, b.FramingTokens, b.InitialPromptTokens, b.ReservedTokens, b.ContextWindow)
				if b.InputAccountingPolicy != "" {
					limits += fmt.Sprintf(" accounting=%s overhead_allowance_tokens=%d host_input_unverified=%t auto_compact_unverified=%t", b.InputAccountingPolicy, b.OverheadAllowanceTokens, b.HostInputUnverified, b.AutoCompactUnverified)
				}
				if b.AdjustmentReason != "" {
					limits += " adjustment=" + b.AdjustmentReason
				}
			}
			if prepared.Deferred != nil {
				fmt.Fprintln(runtime.stderr, "cxt: native runtime prepared; history preparation waits for the initial question (provider acceptance unknown)")
			} else {
				fmt.Fprintf(runtime.stderr, "cxt: mode=%s budget=%d selected_tokens=%d%s code=%s revision=%s capability=%s delivery=prepared (provider acceptance unknown)\n",
					receipt.Mode, receipt.RequestedBudget, receipt.SelectedTokens, limits, receipt.CodeCommit, receipt.SourceRevision, receipt.Capability)
			}
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		ownedStart := prepared.Deferred != nil && prepared.Deferred.owned.start != nil
		var process providerLaunchProcess
		recordLaunched := func() error {
			if !managed {
				return nil
			}
			if err := validatePreparedProviderLaunch(request, prepared); err != nil {
				return err
			}
			receipt.State = "launched"
			if prepared.Deferred != nil {
				receipt.State = "runtime_launched"
			}
			if err := hooks.Record(ctx, cloneProviderLaunchReceipt(receipt)); err != nil {
				if !ownedStart {
					return fmt.Errorf("%w: provider started but its delivery receipt could not be persisted: %w", domain.ErrDeliveryFailed, err)
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				// Owned Start already passed its durable release gate and
				// returned a started TUI. This receipt only observes that start.
				fmt.Fprintln(runtime.stderr, "cxt: native runtime launch receipt was not confirmed saved; the conversation remains active")
			}
			if prepared.Deferred != nil && !ownedStart {
				// The ordinary TUI can submit its argv question early, but its
				// preparation gate opens only after runtime_launched is durable.
				return activateDeferredProviderLaunch(ctx, prepared.Deferred)
			}
			return nil
		}
		if err := deferredProviderFailureReady(prepared.Deferred); err != nil {
			return fail(err)
		}
		if ownedStart {
			// Start can itself release the first turn. Persist and activate
			// before calling it; runtime_launched still means a TUI was started.
			receipt.State = "runtime_starting"
			if err := hooks.Record(ctx, cloneProviderLaunchReceipt(receipt)); err != nil {
				return fail(redactDeferredProviderError(err))
			}
			if err := activateDeferredProviderLaunch(ctx, prepared.Deferred); err != nil {
				return fail(err)
			}
			startOwned := prepared.Deferred.owned.start
			process.start(ctx, func(ctx context.Context) (*exec.Cmd, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				cmd, err := startOwned(ctx, runtime.stdin, runtime.stdout, runtime.stderr, signals.terminalStarted)
				if cmd != nil && cmd.Process != nil {
					signals.terminalStarted()
				}
				return cmd, err
			})
		} else {
			child := exec.Command(bin, args...)
			// Keep provider --cd relative to the original invocation, while using
			// its resolved destination for CXT selection and boundary monitoring.
			child.Dir = cwd
			child.Stdin, child.Stdout, child.Stderr = runtime.stdin, runtime.stdout, runtime.stderr
			if prepared.Deferred != nil {
				child.Env = append([]string{}, prepared.Deferred.Env...)
			} else {
				child.Env = providerLaunchEnvironment(ctx, selectionCwd, intent.Provider, args, supervised)
				if managed {
					child.Env = append(child.Env, "CXT_WRAPPER_TRANSITION_PROTOCOL=prepare-first-v1")
				}
			}
			if err := child.Start(); err != nil {
				return fail(fmt.Errorf("%w: start %s: %w", domain.ErrDeliveryFailed, intent.Provider, err))
			}
			signals.terminalStarted()
			process.adopt(child)
			if err := recordLaunched(); err != nil {
				stopErr := process.stop()
				return fail(errors.Join(err, stopErr))
			}
		}
		var exitErr error
		var runtimeFailure <-chan error
		if prepared.Deferred != nil {
			runtimeFailure = prepared.Deferred.Failure
		}
		transition, retireSession := false, false
		ticker := time.NewTicker(runtime.pollInterval)
	watch:
		for {
			select {
			case result := <-process.starting:
				if err := process.started(result); err != nil {
					exitErr = redactDeferredProviderError(err)
					break watch
				}
				if err := ctx.Err(); err != nil {
					exitErr = err
					break watch
				}
				if err := deferredProviderFailureReady(prepared.Deferred); err != nil {
					exitErr = err
					break watch
				}
				if err := recordLaunched(); err != nil {
					exitErr = err
					break watch
				}
			case failure, open := <-runtimeFailure:
				if !open || failure == nil {
					runtimeFailure = nil
					continue
				}
				exitErr = redactDeferredProviderError(failure)
				break watch
			case exitErr = <-process.done:
				process.done = nil
				if failure := deferredProviderFailureReady(prepared.Deferred); failure != nil {
					exitErr = failure
					break watch
				}
				transition = supervised && newBoundarySince(selectionCwd, start)
				break watch
			case <-ctx.Done():
				exitErr = ctx.Err()
				break watch
			case <-ticker.C:
				if supervised && newBoundarySince(selectionCwd, start) {
					if managed {
						b, ok := boundaryLoad(selectionCwd)
						if !ok {
							continue
						}
						if !providerfs.ValidSessionID(b.SeedID) {
							fmt.Fprintln(runtime.stderr, "cxt: transition has no valid resume target; current session was preserved")
							start, _ = time.Parse(time.RFC3339Nano, b.At)
							continue
						}
						observed := time.Now()
						restart := request
						restart.Intent.ProviderArgs = append([]string(nil), inv.RestartArgs...)
						restart.Transition = &ProviderLaunchTransition{PreviousBranch: b.PrevBranch, Branch: b.Branch, BoundarySeedID: b.SeedID}
						p, r, prepareErr := prepareProviderLaunch(ctx, restart, hooks)
						if prepareErr == nil {
							if p.Deferred == nil {
								warmAgentIndexContext(ctx, bin, intent.Provider, selectionCwd, p.SessionID)
							}
							prepareErr = validateReadyLaunch(ctx, restart, observed, p)
							if prepareErr != nil {
								prepareErr = errors.Join(prepareErr, cleanupProviderLaunch(p))
							}
							if prepareErr != nil && p.Deferred != nil {
								prepareErr = launchFailure(ctx, hooks, r, true, prepareErr)
							}
						}
						if prepareErr != nil {
							if errors.Is(prepareErr, errProviderCleanup) {
								exitErr = prepareErr
								break watch
							}
							fmt.Fprintf(runtime.stderr, "cxt: restart preparation failed; current session was preserved: %v\n", prepareErr)
							// One attempt per boundary. A later Git transition remains
							// observable; never busy-loop failed network preparation.
							start, _ = time.Parse(time.RFC3339Nano, b.At)
							continue
						}
						request = restart
						next = &readyLaunch{prepared: p, receipt: r, observed: observed}
					}
					if failure := deferredProviderFailureReady(prepared.Deferred); failure != nil {
						exitErr = failure
						break watch
					}
					transition = true
					retireSession = managed && prepared.Deferred == nil
					break watch
				}
			}
		}
		ticker.Stop()
		// Join startup and reap any late child before retiring the runtime or
		// starting a prewarmed replacement. One loop owns both lifecycle phases.
		exitErr = errors.Join(exitErr, process.stop())
		if process.startErr != nil {
			transition = false
		}
		if retireSession {
			retireOwnedProviderSession(ctx, selectionCwd, intent.Provider, prepared.SessionID)
		}
		cleanupErr := cleanup()
		if failure := deferredProviderFailureReady(prepared.Deferred); failure != nil {
			exitErr = errors.Join(exitErr, failure)
			transition = false
		}
		if cleanupErr != nil {
			// Retirement must be confirmed before a prewarmed replacement can
			// launch. The outer defer disposes that unused replacement too.
			return launchFailure(ctx, hooks, receipt, managed, errors.Join(exitErr, ctx.Err(), cleanupErr))
		}
		if ctx.Err() != nil {
			return launchFailure(ctx, hooks, receipt, managed, errors.Join(exitErr, ctx.Err()))
		}
		if !transition {
			if exitErr != nil {
				return launchFailure(ctx, hooks, receipt, managed, exitErr)
			}
			// A successful exit does not prove input acceptance or fidelity.
			return nil
		}
		b, ok := boundaryLoad(selectionCwd)
		if !ok || !providerfs.ValidSessionID(b.SeedID) {
			return launchFailure(ctx, hooks, receipt, managed, fmt.Errorf("context changed but no valid resume target is available; delivery stopped"))
		}
		// Return input to CXT only for a replacement, after joining the old TUI.
		// Final shutdown must retain native ownership for queued terminal SIGINT.
		signals.ownedInput()
		fmt.Fprintf(runtime.stderr, "\ncxt: context transition (%s → %s); preparing restart\n", b.PrevBranch, b.Branch)
		if managed {
			// Keep input mode/budget, reselect the new code/revision, and check
			// capability again. Never silently resume a legacy raw seed for B.
			if next == nil {
				request.Intent.ProviderArgs = append([]string(nil), inv.RestartArgs...)
				request.Transition = &ProviderLaunchTransition{PreviousBranch: b.PrevBranch, Branch: b.Branch, BoundarySeedID: b.SeedID}
			}
		} else {
			args = append(resumeArgs(string(intent.Provider), b.SeedID), inv.RestartArgs...)
			warmAgentIndexContext(ctx, bin, string(intent.Provider), selectionCwd, b.SeedID)
		}
	}
}

func validateReadyLaunch(ctx context.Context, request ProviderLaunchRequest, observed time.Time, prepared PreparedProviderLaunch) error {
	if err := deferredProviderFailureReady(prepared.Deferred); err != nil {
		return err
	}
	if newBoundarySince(request.Cwd, observed) {
		return fmt.Errorf("%w: context changed during launch preparation", domain.ErrSelectionChanged)
	}
	if prepared.Validate != nil {
		if err := prepared.Validate(ctx); err != nil {
			if prepared.Deferred != nil {
				return redactDeferredProviderError(err)
			}
			return err
		}
	}
	if err := validatePreparedProviderLaunch(request, prepared); err != nil {
		return err
	}
	if newBoundarySince(request.Cwd, observed) {
		return fmt.Errorf("%w: context changed during launch validation", domain.ErrSelectionChanged)
	}
	if err := deferredProviderFailureReady(prepared.Deferred); err != nil {
		return err
	}
	return ctx.Err()
}

func retireOwnedProviderSession(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID string) {
	if !providerfs.ValidSessionID(sessionID) {
		return
	}
	var paths []string
	if provider == domain.ProviderClaude {
		paths = claudeSessionFiles(cwd)
	} else {
		paths = codexSessionFiles(ctx, cwd)
	}
	for _, path := range paths {
		if providerfs.SessionIDFromPath(path) == sessionID {
			boundary.Supersede(cwd, path)
		}
	}
}

var errProviderCleanup = errors.New("provider runtime cleanup failed")

func cleanupProviderLaunch(p PreparedProviderLaunch) error {
	if p.Cleanup == nil {
		return nil
	}
	if err := p.Cleanup(); err != nil {
		return redactDeferredProviderError(errors.Join(errProviderCleanup, err))
	}
	return nil
}

func prepareProviderLaunch(ctx context.Context, request ProviderLaunchRequest, hooks ProviderLaunchHooks) (PreparedProviderLaunch, ProviderLaunchReceipt, error) {
	if hooks.PrepareDeferred != nil && nativeDeferredProvider(request.Intent.Provider) && request.Intent.Pull {
		return prepareDeferredProviderLaunch(ctx, request, hooks)
	}
	receipt := ProviderLaunchReceipt{Version: 1, Provider: request.Intent.Provider, Mode: "memory", RequestedBudget: domain.DefaultMemoryContextTokens, State: "preparing", Acceptance: "unknown"}
	if request.Intent.Pull {
		receipt.Mode, receipt.RequestedBudget = "history", request.Intent.ContextBudget
	}
	originalArgs := append([]string(nil), request.Intent.ProviderArgs...)
	request.Intent.ProviderArgs = append([]string(nil), originalArgs...)
	prepared, err := hooks.Prepare(ctx, request)
	request.Intent.ProviderArgs = originalArgs
	if prepared.Deferred != nil {
		if prepared.Deferred.Cleanup != nil {
			_ = prepared.Deferred.Cleanup()
		}
		err = fmt.Errorf("%w: deferred runtime requires the deferred preparation hook", domain.ErrDeliveryFailed)
	}
	prepared.Budget = cloneAgentContextBudget(prepared.Budget)
	receipt.Budget = cloneAgentContextBudget(prepared.Budget)
	receipt.PackageHash, receipt.CodeCommit, receipt.SourceRevision = prepared.PackageHash, prepared.CodeCommit, prepared.SourceRevision
	if prepared.Bootstrap != nil {
		proof := *prepared.Bootstrap
		receipt.Bootstrap = &proof
		receipt.Mode = "empty_bootstrap"
	}
	receipt.SelectedTokens, receipt.TokenMeasurement, receipt.Capability = prepared.SelectedTokens, prepared.TokenMeasurement, prepared.Capability
	if err == nil {
		err = validatePreparedProviderLaunch(request, prepared)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		err = errors.Join(err, cleanupProviderLaunch(prepared))
		return prepared, receipt, launchFailure(ctx, hooks, receipt, true, fmt.Errorf("CXT context preparation failed; %s was not started: %w", request.Intent.Provider, err))
	}
	receipt.State = "prepared"
	if err := hooks.Record(ctx, cloneProviderLaunchReceipt(receipt)); err != nil {
		err = errors.Join(err, cleanupProviderLaunch(prepared))
		return prepared, receipt, fmt.Errorf("%w: CXT context was prepared but its receipt could not be persisted; provider was not started: %w", domain.ErrDeliveryFailed, err)
	}
	return prepared, receipt, nil
}

func validatePreparedProviderLaunch(request ProviderLaunchRequest, prepared PreparedProviderLaunch) error {
	if prepared.Deferred != nil {
		return validateDeferredProviderLaunch(request, prepared)
	}
	bootstrap := prepared.Bootstrap
	if bootstrap != nil {
		inv, err := inspectLaunchIntent(request.Intent)
		if err != nil || inv.Mode != providerFresh || request.Intent.Pull || request.Intent.ContextBudget != 0 || request.Intent.WorkStatePath != "" || request.Transition != nil || bootstrap.Validate() != nil || prepared.Validate == nil || prepared.CodeCommit != bootstrap.CodeCommit || prepared.SourceRevision != string(bootstrap.Server.StateHash) || prepared.Capability != "verified_empty_repository" {
			return fmt.Errorf("%w: invalid empty-repository launch proof", domain.ErrDeliveryFailed)
		}
	}
	if domain.ValidateContentHash(prepared.PackageHash) != nil || len(prepared.Args) == 0 || (prepared.CodeCommit == "" && bootstrap == nil) || prepared.SourceRevision == "" {
		return fmt.Errorf("%w: prepared delivery lacks a package hash, code selection, source revision, or invocation", domain.ErrDeliveryFailed)
	}
	if prepared.SelectedTokens < 0 || prepared.TokenMeasurement == "" {
		return fmt.Errorf("%w: prepared delivery lacks a valid token measurement", domain.ErrDeliveryFailed)
	}
	for _, arg := range prepared.Args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("%w: prepared provider argument contains a NUL byte", domain.ErrDeliveryFailed)
		}
	}
	if !providerfs.ValidSessionID(prepared.SessionID) {
		return fmt.Errorf("%w: prepared delivery has no valid materialized session", domain.ErrDeliveryFailed)
	}
	expected := append(resumeArgs(request.Intent.Provider, prepared.SessionID), request.Intent.ProviderArgs...)
	if !slices.Equal(prepared.Args, expected) {
		return fmt.Errorf("%w: prepared invocation changed provider arguments or supplied no verified materialized-session prefix", domain.ErrDeliveryFailed)
	}
	if sessionIDFromAgentArgs(string(request.Intent.Provider), prepared.Args) != prepared.SessionID {
		return fmt.Errorf("%w: prepared invocation does not resume its materialized session", domain.ErrDeliveryFailed)
	}
	if request.Intent.Pull {
		if prepared.Capability != "verified_for_preparation" {
			return fmt.Errorf("%w: history host capability is unverified; select a supported model/host or a smaller verified budget", domain.ErrProviderCapabilityUnknown)
		}
		if prepared.TokenMeasurement != "exact" {
			return fmt.Errorf("%w: history requires exact token accounting", domain.ErrProviderCapabilityUnknown)
		}
		if prepared.Budget == nil {
			return fmt.Errorf("%w: history requires verified preparation budget accounting", domain.ErrProviderCapabilityUnknown)
		}
		inv, err := inspectLaunchIntent(request.Intent)
		if err != nil {
			return err
		}
		model := inv.Model
		if model == "" {
			// The parser's retained settings exclude literal prompts. An empty
			// explicit model must not be treated as a resolved host default.
			for _, arg := range inv.RestartArgs {
				if arg == "--model" || strings.HasPrefix(arg, "--model=") || request.Intent.Provider == domain.ProviderCodex && (arg == "-m" || strings.HasPrefix(arg, "-m=")) {
					return fmt.Errorf("%w: explicit history model must match the resolved preparation model", domain.ErrProviderCapabilityUnknown)
				}
			}
			model = prepared.Budget.Model
		}
		usage := domain.AgentTokenUsage{Tokens: prepared.SelectedTokens, Exact: true, Tokenizer: prepared.Budget.Tokenizer}
		if err := prepared.Budget.Validate(request.Intent.Provider, model, request.Intent.ContextBudget, usage); err != nil {
			return err
		}
		prompt, err := request.InitialPrompt()
		if err != nil {
			return err
		}
		budget := prepared.Budget
		if err := prepared.PromptReservation.Validate(prompt, budget.Provider, budget.Model, budget.Tokenizer, budget.InitialPromptTokens); err != nil {
			return err
		}
	} else if prepared.Budget != nil {
		return fmt.Errorf("%w: preparation budget accounting is only valid for history delivery", domain.ErrDeliveryFailed)
	}
	return nil
}

func cloneAgentContextBudget(budget *domain.AgentContextBudget) *domain.AgentContextBudget {
	if budget == nil {
		return nil
	}
	copy := *budget
	return &copy
}

func cloneProviderLaunchReceipt(receipt ProviderLaunchReceipt) ProviderLaunchReceipt {
	return receipt.Clone()
}

// Clone isolates mutable observation/accounting fields from record callbacks.
func (receipt ProviderLaunchReceipt) Clone() ProviderLaunchReceipt {
	receipt.Budget = cloneAgentContextBudget(receipt.Budget)
	if receipt.NativeInputEstimate != nil {
		copy := *receipt.NativeInputEstimate
		receipt.NativeInputEstimate = &copy
	}
	return receipt
}

func launchFailure(ctx context.Context, hooks ProviderLaunchHooks, receipt ProviderLaunchReceipt, managed bool, cause error) error {
	if !managed || hooks.Record == nil {
		return cause
	}
	deferred := strings.HasPrefix(receipt.State, "runtime_")
	if deferred {
		cause = redactDeferredProviderError(cause)
		receipt.State = "runtime_failed"
	} else {
		receipt.State = "failed"
	}
	receipt.Failure = cause.Error()
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := hooks.Record(recordCtx, cloneProviderLaunchReceipt(receipt)); err != nil {
		if deferred {
			return redactDeferredProviderError(errors.Join(cause, err))
		}
		return errors.Join(cause, fmt.Errorf("%w: persist failed delivery receipt: %w", domain.ErrDeliveryFailed, err))
	}
	return cause
}

func stopProviderChild(child *exec.Cmd, done <-chan error) error {
	_ = child.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = child.Process.Kill()
		return <-done
	}
}

func providerLaunchEnvironment(ctx context.Context, cwd string, provider domain.ProviderKind, args []string, supervised bool) []string {
	var env []string
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if name == "CXT_WRAPPED_CAPTURE_PROTOCOL" || name == "CXT_WRAPPED" || name == "CXT_WRAPPER_PID" || name == "CXT_WRAPPED_AGENT" || name == "CXT_WRAPPED_SESSION_ID" || name == "CXT_WRAPPER_TRANSITION_PROTOCOL" {
			continue
		}
		env = append(env, item)
	}
	if !supervised {
		return env
	}
	env = append(env, "CXT_WRAPPED=1", fmt.Sprintf("CXT_WRAPPER_PID=%d", os.Getpid()), "CXT_WRAPPED_AGENT="+string(provider), "CXT_WRAPPED_SESSION_ID="+sessionIDFromAgentArgs(string(provider), args))
	if provider == domain.ProviderClaude {
		profile := claudeMemoryProfileEnv(cwd, args)
		if len(profile) > 0 && profile[0] == "CXT_CLAUDE_MEMORY_PROFILE=v1" {
			profile = append(profile, "CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT="+memoryadapter.ClaudeMemoryConfigFingerprint(ctx, cwd))
		}
		env = append(env, profile...)
	}
	return env
}
