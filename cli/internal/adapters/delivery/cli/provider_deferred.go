package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// DeferredProviderLaunch owns an already prepared native runtime. Args are the
// exact TUI invocation authorized by Validate, not a materialized-session prefix.
// Env must be the unmodified request.Environment(ctx) used to start that runtime.
// Validate must capture its own immutable invocation and recheck runtime, branch,
// and code selection. No context package or provider acceptance is asserted here.
// Activate is required: preparation/generation must wait until it succeeds. The
// supervisor invokes the owned activation once, only after runtime_launched is
// durably recorded. Activate must revalidate runtime authority before release.
// Cleanup is required, including when preparation returns a partial runtime and
// an error. Failure reports fatal lifecycle errors only; nil/closed channels are
// disabled. Normal child exit and calibration persistence errors are not fatal.
type DeferredProviderLaunch struct {
	Args      []string
	Env       []string
	SessionID string
	Validate  func(context.Context) error
	Activate  func(context.Context) error
	Cleanup   func() error
	Failure   <-chan error
	owned     *deferredProviderInvocation
}

func (DeferredProviderLaunch) String() string     { return "deferred native runtime (private invocation)" }
func (d DeferredProviderLaunch) GoString() string { return d.String() }

type deferredProviderInvocation struct {
	request   ProviderLaunchRequest
	args, env []string
	sessionID string
	activate  func(context.Context) error
}

// Environment returns a fresh copy of the supervised, prepare-first environment.
// During deferred preparation every call returns the same captured environment,
// so the native app-server and TUI cannot inherit different launch configuration.
func (r ProviderLaunchRequest) Environment(ctx context.Context) []string {
	env := r.environment
	if env == nil {
		env = providerLaunchEnvironment(ctx, r.Cwd, r.Intent.Provider, r.Intent.ProviderArgs, true)
		env = append(env, "CXT_WRAPPER_TRANSITION_PROTOCOL=prepare-first-v1")
		if r.Intent.Provider == domain.ProviderCodex && r.Intent.Pull {
			env = append(env, "CXT_WRAPPED_CAPTURE_PROTOCOL="+capture.NativeWrapperCaptureProtocol)
		}
	}
	return append([]string{}, env...)
}

func cloneDeferredRequest(r ProviderLaunchRequest) ProviderLaunchRequest {
	r.Intent.ProviderArgs = append([]string(nil), r.Intent.ProviderArgs...)
	r.environment = append([]string(nil), r.environment...)
	if r.Transition != nil {
		transition := *r.Transition
		r.Transition = &transition
	}
	return r
}

func sameDeferredRequest(a, b ProviderLaunchRequest) bool {
	if a.Cwd != b.Cwd || a.Executable != b.Executable || a.Intent.Provider != b.Intent.Provider ||
		a.Intent.Pull != b.Intent.Pull || a.Intent.ContextBudget != b.Intent.ContextBudget ||
		a.Intent.WorkStatePath != b.Intent.WorkStatePath || !slices.Equal(a.Intent.ProviderArgs, b.Intent.ProviderArgs) {
		return false
	}
	return a.Transition == nil && b.Transition == nil || a.Transition != nil && b.Transition != nil && *a.Transition == *b.Transition
}

func prepareDeferredProviderLaunch(ctx context.Context, request ProviderLaunchRequest, hooks ProviderLaunchHooks) (PreparedProviderLaunch, ProviderLaunchReceipt, error) {
	receipt := ProviderLaunchReceipt{Version: 1, Provider: request.Intent.Provider, Mode: "history", State: "runtime_preparing", Acceptance: "unknown"}
	var prepared PreparedProviderLaunch
	fail := func(err error) (PreparedProviderLaunch, ProviderLaunchReceipt, error) {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
		return prepared, receipt, launchFailure(ctx, hooks, receipt, true, redactDeferredProviderError(err))
	}
	inv, err := inspectLaunchIntent(request.Intent)
	if err != nil || inv.Mode != providerFresh || request.Intent.Provider != domain.ProviderCodex || !request.Intent.Pull || hooks.PrepareDeferred == nil || hooks.Record == nil {
		return fail(domain.ErrDeliveryFailed)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	request = cloneDeferredRequest(request)
	request.environment = request.Environment(ctx)
	hookRequest := cloneDeferredRequest(request)
	d, err := hooks.PrepareDeferred(ctx, hookRequest)
	// Own every mutable value before invoking a receipt or validation callback.
	d.Args = append([]string(nil), d.Args...)
	d.Env = append([]string(nil), d.Env...)
	d.owned = &deferredProviderInvocation{request: cloneDeferredRequest(request), args: append([]string(nil), d.Args...), env: request.Environment(ctx), sessionID: d.SessionID}
	if d.Activate != nil {
		activate := d.Activate
		var once sync.Once
		var activationErr error
		d.owned.activate = func(ctx context.Context) error {
			once.Do(func() {
				activationErr = ctx.Err()
				if activationErr == nil {
					activationErr = activate(ctx)
				}
			})
			return activationErr
		}
		d.Activate = d.owned.activate
	}
	if d.Cleanup != nil {
		cleanup := d.Cleanup
		var once sync.Once
		var cleanupErr error
		d.Cleanup = func() error {
			once.Do(func() { cleanupErr = cleanup() })
			return cleanupErr
		}
	}
	prepared = PreparedProviderLaunch{Deferred: &d, Validate: d.Validate, Cleanup: d.Cleanup}
	if err != nil {
		return fail(err)
	}
	if !sameDeferredRequest(request, hookRequest) {
		return fail(domain.ErrDeliveryFailed)
	}
	if err := validatePreparedProviderLaunch(request, prepared); err != nil {
		return fail(err)
	}
	if err := d.Validate(ctx); err != nil {
		return fail(err)
	}
	if err := deferredProviderFailureReady(&d); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	receipt.State, receipt.SessionID = "runtime_prepared", d.SessionID
	if err := hooks.Record(ctx, cloneProviderLaunchReceipt(receipt)); err != nil {
		return fail(err)
	}
	return prepared, receipt, nil
}

func validateDeferredProviderLaunch(request ProviderLaunchRequest, p PreparedProviderLaunch) error {
	d := p.Deferred
	inv, err := inspectLaunchIntent(request.Intent)
	if err != nil || inv.Mode != providerFresh || request.Intent.Provider != domain.ProviderCodex || !request.Intent.Pull ||
		d == nil || d.owned == nil || d.Validate == nil || d.Activate == nil || d.owned.activate == nil || d.Cleanup == nil || p.Validate == nil || p.Cleanup == nil ||
		!sameDeferredRequest(request, d.owned.request) || !slices.Equal(d.Args, d.owned.args) || !slices.Equal(d.Env, d.owned.env) || d.SessionID != d.owned.sessionID ||
		len(d.Args) == 0 || len(d.Env) == 0 || !providerfs.ValidSessionID(d.SessionID) ||
		p.Bootstrap != nil || p.Budget != nil || len(p.Args) != 0 || p.SessionID != "" || p.PackageHash != "" ||
		p.CodeCommit != "" || p.SourceRevision != "" || p.SelectedTokens != 0 || p.TokenMeasurement != "" || p.Capability != "" ||
		p.PromptReservation.Validate(domain.AgentInitialPrompt{}, "", "", "", 0) != nil {
		return redactDeferredProviderError(domain.ErrDeliveryFailed)
	}
	for _, value := range append(append([]string(nil), d.Args...), d.Env...) {
		if strings.ContainsRune(value, 0) {
			return redactDeferredProviderError(domain.ErrDeliveryFailed)
		}
	}
	return nil
}

func deferredProviderFailureReady(d *DeferredProviderLaunch) error {
	if d == nil || d.Failure == nil {
		return nil
	}
	select {
	case err, open := <-d.Failure:
		if !open || err == nil {
			d.Failure = nil
			return nil
		}
		return redactDeferredProviderError(err)
	default:
		return nil
	}
}

// Preserve errors.Is/As for callers without rendering native errors, paths,
// arguments, configuration, or prompt text in receipts or supervisor output.
type deferredProviderError struct{ cause error }

func (e deferredProviderError) Error() string {
	if errors.Is(e.cause, domain.ErrProviderCapabilityUnknown) {
		return "CXT native history requires a supported runtime model/window binding; use cxt load --context-budget <budget> --output <file> to inspect history"
	}
	return "CXT native runtime lifecycle failed; provider acceptance unknown"
}
func (e deferredProviderError) String() string   { return e.Error() }
func (e deferredProviderError) GoString() string { return e.Error() }
func (e deferredProviderError) Unwrap() []error  { return []error{domain.ErrDeliveryFailed, e.cause} }
func redactDeferredProviderError(err error) error {
	if _, redacted := err.(deferredProviderError); redacted {
		return err
	}
	return deferredProviderError{cause: err}
}
