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
	Record  func(context.Context, ProviderLaunchReceipt) error
}

type ProviderLaunchRequest struct {
	Cwd        string
	Executable string
	Intent     LaunchIntent
	Transition *ProviderLaunchTransition
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
	// Bootstrap proves an authorized empty repository, including a genuine
	// unborn Git branch when CodeCommit is empty. Validate must recheck it.
	Bootstrap        *domain.AgentBootstrapProof
	Args             []string
	SessionID        string
	PackageHash      domain.ContentHash
	CodeCommit       string
	SourceRevision   string
	SelectedTokens   int
	TokenMeasurement string
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
	Bootstrap        *domain.AgentBootstrapProof `json:"bootstrap,omitempty"`
	Version          int                         `json:"version"`
	Provider         domain.ProviderKind         `json:"provider"`
	PackageHash      domain.ContentHash          `json:"package_hash,omitempty"`
	Mode             string                      `json:"mode"`
	RequestedBudget  int                         `json:"requested_budget,omitempty"`
	SelectedTokens   int                         `json:"selected_tokens,omitempty"`
	TokenMeasurement string                      `json:"token_measurement,omitempty"`
	CodeCommit       string                      `json:"code_commit,omitempty"`
	SourceRevision   string                      `json:"source_revision,omitempty"`
	Capability       string                      `json:"capability,omitempty"`
	State            string                      `json:"state"`
	Acceptance       string                      `json:"acceptance"`
	Failure          string                      `json:"failure,omitempty"`
}

// RunProviderLaunch launches CLI providers only. Desktop apps require their
// own handoff/MCP adapter; app commands are ordinary provider passthrough.
func RunProviderLaunch(ctx context.Context, cwd string, intent LaunchIntent, hooks ProviderLaunchHooks) error {
	return runProviderLaunch(ctx, cwd, intent, hooks, providerLaunchRuntime{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		interactive:  providerLaunchTerminal(os.Stdin) && providerLaunchTerminal(os.Stdout),
		pollInterval: time.Second,
	})
}

type providerLaunchRuntime struct {
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	interactive  bool
	pollInterval time.Duration
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

func runProviderLaunch(ctx context.Context, cwd string, intent LaunchIntent, hooks ProviderLaunchHooks, runtime providerLaunchRuntime) error {
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
	if managed && (hooks.Prepare == nil || hooks.Record == nil) {
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
		if next != nil && next.prepared.Cleanup != nil {
			_ = next.prepared.Cleanup()
		}
	}()
	for {
		// Include transitions during preparation, receipt writes and index warming.
		start := time.Now()
		var prepared PreparedProviderLaunch
		var receipt ProviderLaunchReceipt
		cleanup := func() {}
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
			if prepared.Cleanup != nil {
				cleanup = func() { _ = prepared.Cleanup() }
			}
			args = append([]string(nil), prepared.Args...)
			// Preserve native session index registration for newly materialized
			// Codex sessions. This is best effort and never counts as acceptance.
			if !prewarmed {
				warmAgentIndexContext(ctx, bin, intent.Provider, selectionCwd, prepared.SessionID)
			}
			if err = validateReadyLaunch(ctx, selectionCwd, start, prepared); err != nil {
				cleanup()
				return launchFailure(ctx, hooks, receipt, true, err)
			}
			fmt.Fprintf(runtime.stderr, "cxt: mode=%s budget=%d selected_tokens=%d code=%s revision=%s capability=%s delivery=prepared (provider acceptance unknown)\n",
				receipt.Mode, receipt.RequestedBudget, receipt.SelectedTokens, receipt.CodeCommit, receipt.SourceRevision, receipt.Capability)
		}
		if err := ctx.Err(); err != nil {
			cleanup()
			return launchFailure(ctx, hooks, receipt, managed, err)
		}
		child := exec.Command(bin, args...)
		// Keep provider --cd relative to the original invocation, while using
		// its resolved destination for CXT selection and boundary monitoring.
		child.Dir = cwd
		child.Stdin, child.Stdout, child.Stderr = runtime.stdin, runtime.stdout, runtime.stderr
		child.Env = providerLaunchEnvironment(ctx, selectionCwd, intent.Provider, args, supervised)
		if managed {
			child.Env = append(child.Env, "CXT_WRAPPER_TRANSITION_PROTOCOL=prepare-first-v1")
		}
		if err := child.Start(); err != nil {
			cleanup()
			return launchFailure(ctx, hooks, receipt, managed, fmt.Errorf("%w: start %s: %w", domain.ErrDeliveryFailed, intent.Provider, err))
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		if managed {
			receipt.State = "launched"
			if err := hooks.Record(ctx, receipt); err != nil {
				stopProviderChild(child, done)
				cleanup()
				return launchFailure(ctx, hooks, receipt, true, fmt.Errorf("%w: provider started but its delivery receipt could not be persisted: %w", domain.ErrDeliveryFailed, err))
			}
		}
		var exitErr error
		transition := false
		ticker := time.NewTicker(runtime.pollInterval)
	watch:
		for {
			select {
			case exitErr = <-done:
				transition = supervised && newBoundarySince(selectionCwd, start)
				break watch
			case <-ctx.Done():
				stopProviderChild(child, done)
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
							warmAgentIndexContext(ctx, bin, intent.Provider, selectionCwd, p.SessionID)
							prepareErr = validateReadyLaunch(ctx, selectionCwd, observed, p)
							if prepareErr != nil && p.Cleanup != nil {
								_ = p.Cleanup()
							}
						}
						if prepareErr != nil {
							fmt.Fprintf(runtime.stderr, "cxt: restart preparation failed; current session was preserved: %v\n", prepareErr)
							// One attempt per boundary. A later Git transition remains
							// observable; never busy-loop failed network preparation.
							start, _ = time.Parse(time.RFC3339Nano, b.At)
							continue
						}
						request = restart
						next = &readyLaunch{prepared: p, receipt: r, observed: observed}
					}
					exitErr = stopProviderChild(child, done)
					if managed {
						retireOwnedProviderSession(ctx, selectionCwd, intent.Provider, prepared.SessionID)
					}
					transition = true
					break watch
				}
			}
		}
		ticker.Stop()
		cleanup()
		if ctx.Err() != nil {
			return launchFailure(ctx, hooks, receipt, managed, ctx.Err())
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

func validateReadyLaunch(ctx context.Context, cwd string, observed time.Time, prepared PreparedProviderLaunch) error {
	if newBoundarySince(cwd, observed) {
		return fmt.Errorf("%w: context changed during launch preparation", domain.ErrSelectionChanged)
	}
	if prepared.Validate != nil {
		if err := prepared.Validate(ctx); err != nil {
			return err
		}
	}
	if newBoundarySince(cwd, observed) {
		return fmt.Errorf("%w: context changed during launch validation", domain.ErrSelectionChanged)
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

func prepareProviderLaunch(ctx context.Context, request ProviderLaunchRequest, hooks ProviderLaunchHooks) (PreparedProviderLaunch, ProviderLaunchReceipt, error) {
	receipt := ProviderLaunchReceipt{Version: 1, Provider: request.Intent.Provider, Mode: "memory", RequestedBudget: domain.DefaultMemoryContextTokens, State: "preparing", Acceptance: "unknown"}
	if request.Intent.Pull {
		receipt.Mode, receipt.RequestedBudget = "history", request.Intent.ContextBudget
	}
	originalArgs := append([]string(nil), request.Intent.ProviderArgs...)
	request.Intent.ProviderArgs = append([]string(nil), originalArgs...)
	prepared, err := hooks.Prepare(ctx, request)
	request.Intent.ProviderArgs = originalArgs
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
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return prepared, receipt, launchFailure(ctx, hooks, receipt, true, fmt.Errorf("CXT context preparation failed; %s was not started: %w", request.Intent.Provider, err))
	}
	receipt.State = "prepared"
	if err := hooks.Record(ctx, receipt); err != nil {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return prepared, receipt, fmt.Errorf("%w: CXT context was prepared but its receipt could not be persisted; provider was not started: %w", domain.ErrDeliveryFailed, err)
	}
	return prepared, receipt, nil
}

func validatePreparedProviderLaunch(request ProviderLaunchRequest, prepared PreparedProviderLaunch) error {
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
		if prepared.SelectedTokens > request.Intent.ContextBudget {
			return fmt.Errorf("%w: history requires an exact token count within the requested budget (%d selected, %d requested)", domain.ErrContextBudgetExceeded, prepared.SelectedTokens, request.Intent.ContextBudget)
		}
	}
	return nil
}

func launchFailure(ctx context.Context, hooks ProviderLaunchHooks, receipt ProviderLaunchReceipt, managed bool, cause error) error {
	if !managed || hooks.Record == nil {
		return cause
	}
	receipt.State, receipt.Failure = "failed", cause.Error()
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := hooks.Record(recordCtx, receipt); err != nil {
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
		if name == "CXT_WRAPPED" || name == "CXT_WRAPPER_PID" || name == "CXT_WRAPPED_AGENT" || name == "CXT_WRAPPED_SESSION_ID" || name == "CXT_WRAPPER_TRANSITION_PROTOCOL" {
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
