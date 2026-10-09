package main

import (
	"context"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type nativeCodexPackagePrepare func(context.Context, nativecodex.Thread, domain.AgentInitialPrompt) (domain.AgentContextPackage, error)
type nativeCodexPackageValidate func(context.Context, domain.AgentContextPackage) error

// newNativeCodexGenerationPrepare composes a private OpenGenerationHandoff
// callback; it does not open a handoff or activate a public launch route. The
// caller must supply the owned thread returned by this bound launch's session.
// Session/handoff owns lifecycle, turn correlation and generation authorization.
//
// prepare must obtain verified runtime/window evidence and count the supplied
// actual question. validate must recheck that authority, configuration/account
// scope, working position, and latest server main. Neither a package digest nor
// the native host/version/settings strings supply that authority themselves.
// expectedNativeWindow must come from the same runtime evidence, in native
// usable-window units. It may exceed the deliberately smaller packing budget;
// it never raises that budget or establishes provider acceptance.
func newNativeCodexGenerationPrepare(
	bound nativeCodexLaunch, session *nativecodex.Session, expected nativecodex.Thread, expectedHost string, expectedNativeWindow int,
	prepare nativeCodexPackagePrepare, validate nativeCodexPackageValidate, store outbound.AgentInputCalibrationStore,
) (func(context.Context, nativecodex.Thread, string) (nativecodex.PreparedGeneration, error), error) {
	if session == nil {
		return nil, nativeCodexGenerationFailure("runtime binding", nativecodex.ErrState)
	}
	b := nativeCodexGenerationBridge{
		bound: bound, expected: expected, expectedHost: expectedHost, expectedNativeWindow: expectedNativeWindow, host: session.HostIdentity,
		prepare: prepare, validate: validate, store: store,
	}
	if err := b.checkRuntime(expected); err != nil {
		return nil, nativeCodexGenerationFailure("runtime binding", err)
	}
	return b.prepareGeneration, nil
}

// The host accessor is private to this composition so mapping tests need no
// native process. Production construction above always binds Session.HostIdentity.
type nativeCodexGenerationBridge struct {
	bound        nativeCodexLaunch
	expected     nativecodex.Thread
	expectedHost string
	// Native telemetry reports the usable window after its effective percentage.
	// Keep it separate from a more conservative CXTHub packing window.
	expectedNativeWindow int
	host                 func() string
	prepare              nativeCodexPackagePrepare
	validate             nativeCodexPackageValidate
	store                outbound.AgentInputCalibrationStore
}

func (nativeCodexGenerationBridge) String() string {
	return "native Codex generation bridge (private input)"
}
func (b nativeCodexGenerationBridge) GoString() string { return b.String() }

func (b nativeCodexGenerationBridge) checkRuntime(thread nativecodex.Thread) error {
	if b.expectedNativeWindow <= 0 || b.host == nil || b.prepare == nil || b.validate == nil || b.store == nil ||
		domain.ValidateContentHash(b.bound.intent) != nil || b.bound.process.Executable == "" || !filepath.IsAbs(b.bound.process.Cwd) ||
		!nativecodex.SupportedHostIdentity(b.expectedHost) || b.host() != b.expectedHost ||
		thread != b.expected || !domain.ValidSessionID(thread.ID) || thread.Model == "" || thread.ModelProvider == "" ||
		!filepath.IsAbs(thread.Cwd) || filepath.Clean(thread.Cwd) != thread.Cwd ||
		domain.ValidateContentHash(domain.ContentHash(thread.SettingsHash)) != nil {
		return nativecodex.ErrState
	}
	if (b.bound.thread.Model != "" && b.bound.thread.Model != thread.Model) ||
		(b.bound.thread.ModelProvider != "" && b.bound.thread.ModelProvider != thread.ModelProvider) {
		return domain.ErrProviderCapabilityUnknown
	}
	return nil
}

func (b nativeCodexGenerationBridge) checkPackage(p domain.AgentContextPackage, question domain.AgentInitialPrompt) error {
	if err := p.ValidateIdentity(); err != nil {
		return err
	}
	if p.Budget == nil {
		return domain.ErrProviderCapabilityUnknown
	}
	estimated := p.Budget.InputAccountingPolicy == domain.CatalogEstimateReserveV1
	if p.ArtifactOnly || p.Policy.Mode != "history" || p.Capability != p.Budget.ExpectedPreparationCapability() ||
		p.Provider != domain.ProviderCodex || p.Budget.Provider != domain.ProviderCodex ||
		p.Budget.Model != b.expected.Model || p.Budget.HostVersion != b.expectedHost ||
		p.Budget.ContextWindow > b.expectedNativeWindow ||
		(!estimated && p.Budget.InputAccountingPolicy != domain.MeasuredInputReserveV1) ||
		p.Content.Selection.SourcePolicy != domain.AgentSourceLatestMain {
		return domain.ErrProviderCapabilityUnknown
	}
	// ValidateIdentity checks exact package usage and the entire budget. This
	// in-memory reservation separately binds the question's bytes, presence,
	// original count, provider/model and tokenizer; receipts cannot restore it.
	return p.ValidateInitialPrompt(question)
}

func (b nativeCodexGenerationBridge) prepareGeneration(ctx context.Context, thread nativecodex.Thread, submitted string) (nativecodex.PreparedGeneration, error) {
	var empty nativecodex.PreparedGeneration
	if err := ctx.Err(); err != nil {
		return empty, nativeCodexGenerationFailure("preparation", err)
	}
	if err := b.checkRuntime(thread); err != nil {
		return empty, nativeCodexGenerationFailure("runtime binding", err)
	}
	// The relay already normalized this question. Never trim/normalize it again,
	// and never reserve an absent prompt for interactive input learned here.
	if b.bound.prompt.Present() && b.bound.prompt.Text() != submitted {
		return empty, nativeCodexGenerationFailure("initial question binding", domain.ErrContextBudgetExceeded)
	}
	question := domain.NewAgentInitialPrompt(submitted)
	p, err := b.prepare(ctx, thread, question)
	if err != nil {
		return empty, nativeCodexGenerationFailure("package preparation", err)
	}
	if err := b.checkPackage(p, question); err != nil {
		return empty, nativeCodexGenerationFailure("package binding", err)
	}
	// Own all slices and pointers, while deliberately retaining the private
	// in-memory reservation that JSON receipts cannot restore by themselves.
	p, err = cloneAgentContextPackage(p)
	if err != nil {
		return empty, nativeCodexGenerationFailure("package snapshot", err)
	}
	text, err := p.Prompt()
	if err != nil {
		return empty, nativeCodexGenerationFailure("package text", err)
	}
	history := []nativecodex.HistoryMessage{{Role: "user", Text: text}}
	checkSnapshot := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return nativeCodexGenerationFailure("revalidation", err)
		}
		if err := b.checkRuntime(thread); err != nil {
			return nativeCodexGenerationFailure("runtime revalidation", err)
		}
		if err := b.checkPackage(p, question); err != nil {
			return nativeCodexGenerationFailure("package revalidation", err)
		}
		if history[0].Role != "user" || history[0].Text != text {
			return nativeCodexGenerationFailure("history binding", domain.ErrHashMismatch)
		}
		return nil
	}
	validate := func(ctx context.Context) error {
		if err := checkSnapshot(ctx); err != nil {
			return err
		}
		candidate, err := cloneAgentContextPackage(p)
		if err != nil {
			return nativeCodexGenerationFailure("package snapshot", err)
		}
		if err := b.validate(ctx, candidate); err != nil {
			return nativeCodexGenerationFailure("source and capability revalidation", err)
		}
		if err := b.checkPackage(candidate, question); err != nil {
			return nativeCodexGenerationFailure("revalidation changed package", err)
		}
		return checkSnapshot(ctx)
	}
	if err := validate(ctx); err != nil {
		return empty, err
	}
	estimated := p.Budget.InputAccountingPolicy == domain.CatalogEstimateReserveV1
	var scope domain.ContentHash
	if !estimated {
		scope, err = nativeCodexGenerationCalibrationScope(*p.Budget)
		if err != nil {
			return empty, nativeCodexGenerationFailure("calibration scope", err)
		}
	}
	// Budget validation bounded both terms inside InitialInputLimit. This sum
	// excludes host/framing/reserve tokens, which native total input measures.
	submittedTokens := p.Usage.Tokens + p.Budget.InitialPromptTokens
	observe := func(ctx context.Context, observed nativecodex.GenerationObservation) error {
		if err := ctx.Err(); err != nil {
			return nativeCodexGenerationFailure("observation", err)
		}
		if observed.ThreadID != thread.ID || !domain.ValidSessionID(observed.TurnID) {
			return nativeCodexGenerationFailure("observation identity", domain.ErrHashMismatch)
		}
		if estimated {
			// A cache estimate is not a runtime descriptor. A different observed
			// window is recorded in the launch journal, never a fatal session
			// error or authorization to replay the already-submitted question.
			// Inexact text counts must not be used to calibrate token overhead.
			if observed.ModelContextWindow < 0 || observed.TotalInputTokens < 0 {
				return nativeCodexGenerationFailure("observation counts", domain.ErrProviderCapabilityUnknown)
			}
			return checkSnapshot(ctx)
		}
		if observed.Ineligible || observed.ModelRerouted {
			return nil // no feedback may cross the prepared runtime/model scope
		}
		if observed.ModelContextWindow < 0 || (observed.ModelContextWindow != 0 && observed.ModelContextWindow != b.expectedNativeWindow) {
			return nativeCodexGenerationFailure("observation window disagreement", domain.ErrProviderCapabilityUnknown)
		}
		// Feedback belongs to the immutable package actually submitted. Source
		// freshness is checked at release, before/after injection; normal model
		// tool activity may change the working tree after that boundary.
		if err := checkSnapshot(ctx); err != nil {
			return err
		}
		input := domain.AgentInputObservation{
			Scope: scope, PackageID: p.ID, Outcome: observed.Outcome, InitialRequest: true,
			ExecutionKnown: observed.ExecutionKnown, ExecutionStarted: observed.ExecutionStarted,
			UsageKnown: observed.UsageKnown, UsageBeforeCompaction: observed.UsageBeforeCompaction,
			TotalInputTokens: observed.TotalInputTokens, SubmittedTextTokens: submittedTokens, SubmittedTextExact: true,
		}
		// Unknown outcomes are recorded through the same validated use case;
		// absent native usage stays absent. Retry eligibility is never acted on.
		recorder := &nativeCodexCalibrationRecorder{AgentInputCalibrationStore: b.store}
		_, _, err := app.RecordAgentInputObservation(ctx, recorder, p, input)
		if err != nil {
			if recorder.attempted {
				return nativeCodexCalibrationPersistenceError{nativeCodexGenerationError{
					stage: "observation persistence", cause: err,
				}}
			}
			return nativeCodexGenerationFailure("observation binding", err)
		}
		return nil
	}
	return nativecodex.PreparedGeneration{History: history, Validate: validate, Observe: observe}, nil
}

// RecordAgentInputObservation validates before calling Merge. Track that
// boundary so a malformed observation is never mislabeled as a cache outage.
type nativeCodexCalibrationRecorder struct {
	outbound.AgentInputCalibrationStore
	attempted bool
}

func (s *nativeCodexCalibrationRecorder) MergeAgentInputCalibration(ctx context.Context, c domain.AgentInputCalibration) (domain.AgentInputCalibration, error) {
	s.attempted = true
	return s.AgentInputCalibrationStore.MergeAgentInputCalibration(ctx, c)
}

// The handoff can recognize this private error through
// interface { CalibrationPersistenceFailure() bool }. It must surface the
// recording failure separately from generation and keep the productive native
// session alive. It must not claim persistence, capacity, or retry authorization.
// A write may already have happened; this bridge never retries it automatically.
type nativeCodexCalibrationPersistenceError struct {
	nativeCodexGenerationError
}

func (nativeCodexCalibrationPersistenceError) CalibrationPersistenceFailure() bool { return true }

// Reconstruct only the domain's scope digest inputs after external validation.
// This is the same integrity operation as AgentContextBudget.capability; the
// temporary Verified/Evidence fields cannot attest a runtime or enable a route.
func nativeCodexGenerationCalibrationScope(b domain.AgentContextBudget) (domain.ContentHash, error) {
	c := domain.AgentHostCapability{
		Provider: b.Provider, Model: b.Model, HostVersion: b.HostVersion, Tokenizer: b.Tokenizer,
		ContextWindow: b.ContextWindow, RuntimeScope: b.RuntimeScope,
		Verified: true, Evidence: "validated preparation scope reconstruction",
	}
	return c.CalibrationScope()
}

// Preserve errors.Is/As (including write failures) without exposing an injected
// callback/store's error text, which can include prompts, paths or credentials.
type nativeCodexGenerationError struct {
	stage string
	cause error
}

func (e nativeCodexGenerationError) Error() string {
	return "native Codex generation " + e.stage + " failed"
}
func (e nativeCodexGenerationError) Unwrap() error    { return e.cause }
func (e nativeCodexGenerationError) String() string   { return e.Error() }
func (e nativeCodexGenerationError) GoString() string { return e.Error() }

func nativeCodexGenerationFailure(stage string, cause error) error {
	return nativeCodexGenerationError{stage: stage, cause: cause}
}
