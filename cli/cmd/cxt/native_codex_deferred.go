package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Runtime readiness is separate from input delivery. The original TUI supplies
// its first question; only then do we authorize and assemble latest server main.
// Static native catalogs are supported by the binding, not manufactured here.
func prepareNativeCodexDeferred(ctx context.Context, cfg config, req delivcli.ProviderLaunchRequest) (result delivcli.DeferredProviderLaunch, resultErr error) {
	var empty delivcli.DeferredProviderLaunch
	if req.Intent.Provider != domain.ProviderCodex || !req.Intent.Pull {
		return empty, domain.ErrUnsupportedProvider
	}
	bound, err := bindNativeCodexLaunch(req)
	if err != nil {
		return empty, err
	}
	preparer, _ := runtimeAgentLoader(cfg)
	checkPosition, err := pinNativeWorkingPosition(ctx, preparer, req.Cwd)
	if err != nil {
		return empty, err
	}
	env := req.Environment(ctx)
	life, cancel := context.WithCancel(ctx)
	session, thread, reader, err := bound.startWithWindow(life, env)
	if err != nil {
		cancel()
		return empty, fmt.Errorf("%w: %w", domain.ErrProviderCapabilityUnknown, err)
	}
	var handoff *nativecodex.Handoff
	captureCleanup := func() error { return nil }
	cleanup := nativeCodexRuntimeCleanup(cancel, func() error {
		if handoff != nil {
			return handoff.Close()
		}
		return nil
	}, session.Close, func() error {
		if captureCleanup != nil {
			return captureCleanup()
		}
		return nil
	})
	ready := false
	defer func() {
		if !ready {
			resultErr = errors.Join(resultErr, cleanup())
		}
	}()
	boundCapture, err := capture.BindNativeWrapperSession(cfg.RepoRoot, os.Getpid(), thread.ID)
	if err != nil {
		return empty, nativeCodexGenerationFailure("capture ownership", err)
	}
	captureCleanup = boundCapture
	preparer.capabilities = reader
	validateRuntime := func(ctx context.Context) error {
		if err := checkPosition(ctx); err != nil {
			return err
		}
		if _, err := reader.AgentCapability(ctx, domain.ProviderCodex, thread.Model); err != nil {
			return err
		}
		return checkPosition(ctx)
	}
	policy := domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: req.Intent.ContextBudget, Source: "explicit_cli"}
	record := func(ctx context.Context, receipt delivcli.ProviderLaunchReceipt) error {
		return recordProviderLaunchReceipt(ctx, cfg.RepoRoot, receipt)
	}
	// This callback is private to one handoff and invoked at most once.
	var selected domain.AgentContextPackage
	activated := make(chan struct{})
	var activateOnce sync.Once
	activate := func(ctx context.Context) error {
		if err := validateRuntime(ctx); err != nil {
			return err
		}
		activateOnce.Do(func() { close(activated) })
		return nil
	}
	prepare := func(ctx context.Context, actual nativecodex.Thread, prompt domain.AgentInitialPrompt) (domain.AgentContextPackage, error) {
		// An argv question can arrive before child.Start's receipt is durable.
		// The supervisor authorizes this gate only after runtime_launched.
		select {
		case <-ctx.Done():
			return domain.AgentContextPackage{}, ctx.Err()
		case <-activated:
		}
		if actual != thread {
			return domain.AgentContextPackage{}, domain.ErrProviderCapabilityUnknown
		}
		if err := validateRuntime(ctx); err != nil {
			return domain.AgentContextPackage{}, err
		}
		p, err := preparer.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{
			Cwd: req.Cwd, Provider: domain.ProviderCodex, Model: thread.Model, Policy: policy,
			WorkStatePath: req.Intent.WorkStatePath, InitialPrompt: prompt,
		})
		if err != nil {
			return p, err
		}
		selected, err = cloneAgentContextPackage(p)
		return p, err
	}
	validate := func(ctx context.Context, p domain.AgentContextPackage) error {
		if err := validateRuntime(ctx); err != nil {
			return err
		}
		if err := validateNativeMeasuredBudget(ctx, app.MeasuredAgentCapabilities{Runtime: reader, Observations: preparer.store}, p); err != nil {
			return err
		}
		return preparer.validateAgentDelivery(ctx, req.Cwd, p.Content.Selection)
	}
	gate, err := newNativeCodexWindowGenerationPrepare(bound, session, reader, prepare, validate, preparer.store)
	if err != nil {
		return empty, err
	}
	handoff, err = session.OpenGenerationHandoff(life, func(ctx context.Context, thread nativecodex.Thread, question string) (nativecodex.PreparedGeneration, error) {
		p, err := gate(ctx, thread, question)
		if err != nil {
			return p, err
		}
		return persistNativeGeneration(ctx, cfg.RepoRoot, thread, selected, p, record)
	})
	if err != nil {
		return empty, err
	}
	args, err := bound.tuiResumeArgs(handoff.URL(), thread)
	if err != nil {
		return empty, err
	}
	if bound.prompt.Present() {
		// Keep literal option-looking prompts after --. The native TUI sends
		// this same normalized text through the delayed first-question gate.
		args = append(args, "--", bound.prompt.Text())
	}
	if err := validateRuntime(ctx); err != nil {
		return empty, err
	}
	failures := monitorNativeCodexLifecycle(life, handoff, func() {
		fmt.Fprintln(os.Stderr, "cxt: native input feedback was not confirmed saved; the conversation remains active")
	})
	ready = true
	return delivcli.DeferredProviderLaunch{Args: args, Env: env, SessionID: thread.ID, Validate: validateRuntime, Activate: activate, Cleanup: cleanup, Failure: failures}, nil
}

func nativeCodexRuntimeCleanup(cancel context.CancelFunc, closers ...func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			cancel()
			for _, close := range closers {
				result = errors.Join(result, close())
			}
			if result != nil {
				result = nativeCodexGenerationFailure("runtime cleanup", result)
			}
		})
		return result
	}
}

type nativeCodexLifecycle interface {
	WaitGeneration(context.Context) (nativecodex.GenerationObservation, error)
	WaitLifecycle(context.Context) error
}

func monitorNativeCodexLifecycle(ctx context.Context, h nativeCodexLifecycle, warn func()) <-chan error {
	failures := make(chan error, 1)
	go func() {
		defer close(failures)
		_, err := h.WaitGeneration(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, nativecodex.ErrCalibrationPersistence) {
			warn()
			err = nil
		}
		if err == nil {
			err = h.WaitLifecycle(ctx)
		}
		if ctx.Err() != nil {
			return // owner cleanup, not an unexpected transport failure
		}
		if err == nil {
			err = nativecodex.ErrClosed
		}
		failures <- nativeCodexGenerationFailure("native lifecycle", err)
	}()
	return failures
}

// Pin local code/worktree only. Server main is intentionally read later, when
// the actual question arrives. An idle TUI never pins a stale shared source.
func pinNativeWorkingPosition(ctx context.Context, r runtimeAgentPreparer, cwd string) (func(context.Context) error, error) {
	repo, err := r.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	code, ok := r.git.(outbound.CodePosition)
	if !ok || r.store == nil {
		return nil, domain.ErrAgentContextUnavailable
	}
	commit, err := code.CurrentCommit(ctx, cwd)
	if err != nil || !domain.ValidGitOID(commit) {
		return nil, domain.ErrCodePositionMismatch
	}
	_, state, _, err := r.agentWorktreeState(ctx, cwd, repo.ID)
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context) error {
		current, err := r.git.CurrentRepo(ctx, cwd)
		if err != nil {
			return err
		}
		if current.ID != repo.ID {
			return domain.ErrHashMismatch
		}
		if err := r.validateAgentWorktree(ctx, cwd, repo.ID, state); err != nil {
			return err
		}
		actual, err := code.CurrentCommit(ctx, cwd)
		if err != nil {
			return err
		}
		if actual != commit {
			return domain.ErrCodePositionMismatch
		}
		return ctx.Err()
	}
	return check, check(ctx)
}

func validateNativeMeasuredBudget(ctx context.Context, reader outbound.AgentCapabilityReader, p domain.AgentContextPackage) error {
	if reader == nil || p.Budget == nil {
		return domain.ErrProviderCapabilityUnknown
	}
	c, err := reader.AgentCapability(ctx, p.Provider, p.Budget.Model)
	if err != nil {
		return err
	}
	c.InitialPromptTokens = p.Budget.InitialPromptTokens
	b, err := c.ResolveBudget(p.Provider, p.Budget.Model, p.Policy.BudgetTokens, p.Usage)
	if err != nil {
		return err
	}
	if b != *p.Budget {
		return domain.ErrProviderCapabilityUnknown
	}
	return nil
}

// The journal distinguishes a prepared package, acknowledged injection ready
// for release, and a correlated first-turn outcome. No successful process start
// or injection ACK is written as model acceptance. Original question/config
// and the transport's raw-history hash stay out of routine receipts.
func persistNativeGeneration(ctx context.Context, root string, thread nativecodex.Thread, selected domain.AgentContextPackage, prepared nativecodex.PreparedGeneration,
	record func(context.Context, delivcli.ProviderLaunchReceipt) error,
) (nativecodex.PreparedGeneration, error) {
	if record == nil || prepared.Validate == nil || prepared.Observe == nil || len(prepared.History) != 1 || selected.Budget == nil {
		return nativecodex.PreparedGeneration{}, domain.ErrDeliveryFailed
	}
	p, err := cloneAgentContextPackage(selected)
	if err != nil {
		return nativecodex.PreparedGeneration{}, err
	}
	text, err := p.Prompt()
	if err != nil || prepared.History[0].Role != "user" || prepared.History[0].Text != text {
		return nativecodex.PreparedGeneration{}, domain.ErrHashMismatch
	}
	if err = persistAgentInputPackage(ctx, root, p); err != nil {
		return nativecodex.PreparedGeneration{}, nativeCodexGenerationFailure("package persistence", err)
	}
	receipt := delivcli.ProviderLaunchReceipt{Version: 1, Provider: p.Provider, Mode: p.Policy.Mode, RequestedBudget: p.Policy.BudgetTokens,
		SessionID:   thread.ID,
		PackageHash: p.ID, CodeCommit: p.Content.Selection.DeliveryCodeCommit(), SourceRevision: string(p.Content.Selection.ContextStateHash),
		SelectedTokens: p.Usage.Tokens, TokenMeasurement: "exact", Capability: p.Capability, Budget: p.Budget,
		State: "package_prepared", Acceptance: "unknown"}
	if err = record(ctx, receipt); err != nil {
		return nativecodex.PreparedGeneration{}, nativeCodexGenerationFailure("preparation receipt", err)
	}
	prepared.BeforeRelease = func(ctx context.Context, injection nativecodex.InjectionReceipt) error {
		if injection.ThreadID != thread.ID || !injection.Acknowledged || injection.Items != 1 || injection.UTF8Bytes != len(text) || injection.ProviderAcceptance != "unverified" {
			return domain.ErrHashMismatch
		}
		r := receipt
		r.State = "injected_ready"
		if err := record(ctx, r); err != nil {
			return nativeCodexGenerationFailure("injection receipt", err)
		}
		return nil
	}
	observe := prepared.Observe
	prepared.Observe = func(ctx context.Context, o nativecodex.GenerationObservation) error {
		err := observe(ctx, o)
		if err != nil && !errors.Is(err, nativecodex.ErrCalibrationPersistence) {
			var persistence interface{ CalibrationPersistenceFailure() bool }
			if !errors.As(err, &persistence) || !persistence.CalibrationPersistenceFailure() {
				return err
			}
		}
		r := receipt
		r.State = "first_turn_observed"
		r.TurnID, r.Outcome = o.TurnID, o.Outcome
		// Completion is recorded separately from fidelity or full-history use.
		if o.Outcome == "completed" && !o.Ineligible && !o.ModelRerouted {
			r.Acceptance = "first_turn_completed"
		}
		if writeErr := record(ctx, r); writeErr != nil {
			return nativeCodexCalibrationPersistenceError{nativeCodexGenerationError{stage: "outcome receipt", cause: writeErr}}
		}
		return err
	}
	return prepared, nil
}
