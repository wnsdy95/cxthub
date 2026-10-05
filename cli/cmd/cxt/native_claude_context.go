package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// This composition reuses the common main selection and the owned exchange.
// It remains private until natural prompt expansion and tool interactions are
// supported. In particular, an estimate receipt never enables public delivery.
type nativeClaudeContextReader struct {
	baseline nativeclaude.ContextSummary
	host     string
	scope    domain.ContentHash
	read     func(context.Context) (nativeclaude.ContextSummary, error)
}

func newNativeClaudeContextReader(ctx context.Context, e *nativeclaude.FirstExchange) (nativeClaudeContextReader, error) {
	if e == nil || e.HostVersion() != "2.1.287" {
		return nativeClaudeContextReader{}, domain.ErrProviderCapabilityUnknown
	}
	s, err := e.ContextSummary(ctx)
	if err != nil {
		return nativeClaudeContextReader{}, redactNativeClaudeContext(err)
	}
	if s.SessionID != e.SessionID() {
		return nativeClaudeContextReader{}, domain.ErrProviderCapabilityUnknown
	}
	r := nativeClaudeContextReader{baseline: s, host: e.HostVersion(), read: e.ContextSummary}
	// Scope belongs only to this owned invocation. It is not an account or
	// configuration fingerprint suitable for sharing observations across runs.
	raw, _ := json.Marshal(struct {
		Host    string
		Summary nativeclaude.ContextSummary
	}{r.host, s})
	r.scope = domain.HashContent(raw)
	_, err = r.capability(s.Model)
	return r, err
}

func (r nativeClaudeContextReader) AgentCapability(ctx context.Context, provider domain.ProviderKind, model string) (domain.AgentHostCapability, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentHostCapability{}, err
	}
	if provider != domain.ProviderClaude || r.read == nil {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	current, err := r.read(ctx)
	if err != nil {
		return domain.AgentHostCapability{}, redactNativeClaudeContext(err)
	}
	if !reflect.DeepEqual(current, r.baseline) {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	return r.capability(model)
}

func (r nativeClaudeContextReader) capability(model string) (domain.AgentHostCapability, error) {
	s := r.baseline
	if r.host != "2.1.287" || !domain.ValidSessionID(s.SessionID) || s.Measurement != "local_estimate" ||
		s.Model == "" || model != s.Model || s.MaxTokens <= 0 || s.RawMaxTokens <= 0 || s.TotalTokens < 0 ||
		s.MaxTokens > int64(^uint(0)>>1) || s.RawMaxTokens > int64(^uint(0)>>1) || s.TotalTokens > int64(^uint(0)>>1) ||
		domain.ValidateContentHash(r.scope) != nil {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	c := domain.AgentHostCapability{
		Provider: domain.ProviderClaude, Model: s.Model, HostVersion: r.host, Verified: true,
		Evidence:      "owned native context summary; local estimates and UTF-8 allowance; provider acceptance unverified",
		ContextWindow: int(min(s.MaxTokens, s.RawMaxTokens)), Tokenizer: domain.UTF8ByteBoundCounter,
		InputAccountingPolicy: domain.NativeEstimateReserveV1, RuntimeScope: r.scope,
		BaselineInputEstimateTokens: int(s.TotalTokens),
		BaselineInputMeasurement:    domain.NativeLocalEstimate,
		// Pinned2.1.287:48-byte reference prefix plus one provider wire LF.
		// This is allowance, not an exact native token count.
		FramingAllowanceTokens: 49,
	}
	if !s.AutoCompactEnabled {
		c.AutoCompactKnown = true
	} else if s.AutoCompactThreshold != nil {
		if *s.AutoCompactThreshold <= 0 || *s.AutoCompactThreshold > int64(^uint(0)>>1) {
			return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
		}
		c.AutoCompactKnown, c.AutoCompactTokens = true, int(*s.AutoCompactThreshold)
	}
	return c, nil
}

func (r nativeClaudeContextReader) checkPackage(p domain.AgentContextPackage, question domain.AgentInitialPrompt) (string, error) {
	if err := p.ValidateIdentity(); err != nil {
		return "", err
	}
	if p.ArtifactOnly || p.Policy.Mode != "history" || p.Provider != domain.ProviderClaude || p.Budget == nil ||
		p.Capability != "verified_for_preparation" || p.Content.Selection.SourcePolicy != domain.AgentSourceLatestMain ||
		p.Content.Selection.Branch != "main" {
		return "", domain.ErrProviderCapabilityUnknown
	}
	c, err := r.capability(p.Budget.Model)
	if err != nil {
		return "", err
	}
	c.InitialPromptTokens = len(question.Text())
	b, err := c.ResolveBudget(p.Provider, c.Model, p.Policy.BudgetTokens, p.Usage)
	if err != nil || b != *p.Budget {
		return "", domain.ErrProviderCapabilityUnknown
	}
	if err := p.ValidateInitialPrompt(question); err != nil {
		return "", err
	}
	text, err := p.Prompt()
	if err != nil {
		return "", err
	}
	if p.Usage.Tokens != len(text) || p.Usage.Exact || p.Usage.Tokenizer != domain.UTF8ByteBoundCounter {
		return "", domain.ErrHashMismatch
	}
	return text, nil
}

func (r nativeClaudeContextReader) admit(p domain.AgentContextPackage, question domain.AgentInitialPrompt, text string, reference nativeclaude.ReferenceReceipt, evidence nativeclaude.FirstQuestionEvidence) error {
	if _, err := r.checkPackage(p, question); err != nil {
		return err
	}
	s := evidence.Summary
	baseline := r.baseline
	// Retained reference changes total usage, not model/window/compaction scope.
	baseline.TotalTokens = s.TotalTokens
	if !reflect.DeepEqual(s, baseline) || s.TotalTokens < r.baseline.TotalTokens || s.TotalTokens > int64(^uint(0)>>1) ||
		evidence.Reference != reference || !reference.NoTurnAcknowledged || reference.Persisted ||
		reference.SessionID != s.SessionID || !domain.ValidSessionID(reference.MessageID) ||
		reference.PayloadHash != string(domain.HashContent([]byte(text))) || reference.UTF8Bytes != len(text) ||
		reference.ProviderAcceptance != "unverified" ||
		evidence.QuestionBytes != len(question.Text()) || evidence.QuestionHash != string(domain.HashContent([]byte(question.Text()))) {
		return domain.ErrHashMismatch
	}
	return p.Budget.ValidateNativeInputEstimate(p.Usage, r.inputEstimate(s))
}

type nativeClaudePreparedInput struct {
	run func(context.Context) (nativeclaude.FirstExchangeResult, error)
}

func (nativeClaudePreparedInput) String() string {
	return "prepared native Claude input (private content)"
}
func (p nativeClaudePreparedInput) GoString() string { return p.String() }

// Caller owns the already-started native process. Preparation never sends a
// querying message. The returned one-shot run uses the existing exchange and
// leaves archive verification/resume to its established lifecycle.
func prepareNativeClaudeInput(ctx context.Context, cfg config, req delivcli.ProviderLaunchRequest, e *nativeclaude.FirstExchange, question domain.AgentInitialPrompt) (prepared nativeClaudePreparedInput, resultErr error) {
	defer func() { resultErr = redactNativeClaudeContext(resultErr) }()
	if req.Intent.Provider != domain.ProviderClaude || !req.Intent.Pull || !question.Present() || question.Text() == "" {
		return nativeClaudePreparedInput{}, domain.ErrDeliveryFailed
	}
	r, err := newNativeClaudeContextReader(ctx, e)
	if err != nil {
		return nativeClaudePreparedInput{}, err
	}
	preparer, _ := runtimeAgentLoader(cfg)
	preparer.capabilities = r
	p, err := preparer.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{
		Cwd: req.Cwd, Provider: domain.ProviderClaude, Model: r.baseline.Model,
		Policy:        domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: req.Intent.ContextBudget, Source: "explicit_cli"},
		WorkStatePath: req.Intent.WorkStatePath, InitialPrompt: question,
	})
	if err != nil {
		return nativeClaudePreparedInput{}, err
	}
	validate := func(ctx context.Context) error {
		return preparer.validateAgentDelivery(ctx, req.Cwd, p.Content.Selection)
	}
	return r.prepareRun(ctx, e, p, question, validate, func(ctx context.Context, receipt delivcli.ProviderLaunchReceipt) error {
		return recordProviderLaunchReceipt(ctx, cfg.RepoRoot, receipt)
	}, func(ctx context.Context, p domain.AgentContextPackage) error {
		return persistAgentInputPackage(ctx, cfg.RepoRoot, p)
	})
}

// A narrow private interface permits deterministic boundary tests. Production
// constructs it only with the same FirstExchange used to read the capability.
type nativeClaudeExchangeRunner interface {
	AppendReference(context.Context, string) (nativeclaude.ReferenceReceipt, error)
	Run(context.Context, string, func(context.Context, nativeclaude.FirstQuestionEvidence) error) (nativeclaude.FirstExchangeResult, error)
	Close() error
}

func (r nativeClaudeContextReader) prepareRun(ctx context.Context, exchange nativeClaudeExchangeRunner, original domain.AgentContextPackage, question domain.AgentInitialPrompt, validate func(context.Context) error, record func(context.Context, delivcli.ProviderLaunchReceipt) error, persist func(context.Context, domain.AgentContextPackage) error) (prepared nativeClaudePreparedInput, resultErr error) {
	defer func() { resultErr = redactNativeClaudeContext(resultErr) }()
	if exchange == nil || validate == nil || record == nil || persist == nil {
		return nativeClaudePreparedInput{}, domain.ErrDeliveryFailed
	}
	p, err := cloneAgentContextPackage(original)
	if err != nil {
		return nativeClaudePreparedInput{}, err
	}
	text, err := r.checkPackage(p, question)
	if err != nil {
		return nativeClaudePreparedInput{}, err
	}
	if err := validate(ctx); err != nil {
		return nativeClaudePreparedInput{}, err
	}
	toStore, err := cloneAgentContextPackage(p)
	if err != nil {
		return nativeClaudePreparedInput{}, err
	}
	if err := persist(ctx, toStore); err != nil {
		return nativeClaudePreparedInput{}, err
	}
	receipt := delivcli.ProviderLaunchReceipt{Version: 1, Provider: p.Provider, Mode: p.Policy.Mode,
		RequestedBudget: p.Policy.BudgetTokens, SessionID: r.baseline.SessionID,
		PackageHash: p.ID, CodeCommit: p.Content.Selection.DeliveryCodeCommit(), SourceRevision: string(p.Content.Selection.ContextStateHash),
		SelectedTokens: p.Usage.Tokens, TokenMeasurement: string(domain.AgentTextUTF8Allowance), Capability: p.Capability, Budget: p.Budget,
		State: "package_prepared", Acceptance: "unknown"}
	if err := record(ctx, receipt.Clone()); err != nil {
		return nativeClaudePreparedInput{}, err
	}
	var attempted atomic.Bool
	return nativeClaudePreparedInput{run: func(ctx context.Context) (result nativeclaude.FirstExchangeResult, resultErr error) {
		if !attempted.CompareAndSwap(false, true) {
			return nativeclaude.FirstExchangeResult{}, domain.ErrDeliveryFailed
		}
		defer func() {
			resultErr = errors.Join(resultErr, exchange.Close())
			resultErr = redactNativeClaudeContext(resultErr)
		}()
		if err := ctx.Err(); err != nil {
			return nativeclaude.FirstExchangeResult{}, err
		}
		if _, bounded := ctx.Deadline(); !bounded {
			return nativeclaude.FirstExchangeResult{}, domain.ErrDeliveryFailed
		}
		if _, err := r.AgentCapability(ctx, p.Provider, p.Budget.Model); err != nil {
			return nativeclaude.FirstExchangeResult{}, err
		}
		if err := validate(ctx); err != nil {
			return nativeclaude.FirstExchangeResult{}, err
		}
		reference, err := exchange.AppendReference(ctx, text)
		if err != nil {
			return nativeclaude.FirstExchangeResult{}, err
		}
		result, err = exchange.Run(ctx, question.Text(), func(ctx context.Context, evidence nativeclaude.FirstQuestionEvidence) error {
			if err := r.admit(p, question, text, reference, evidence); err != nil {
				return err
			}
			ready := receipt.Clone()
			ready.State = "injected_ready"
			estimate := r.inputEstimate(evidence.Summary)
			ready.NativeInputEstimate = &estimate
			if err := record(ctx, ready); err != nil {
				return err
			}
			// Last external operation inside admission: fresh source/code read.
			// This is an observed boundary, not an atomic Git/cloud/model commit.
			if err := validate(ctx); err != nil {
				return err
			}
			return ctx.Err()
		})
		if err != nil {
			return result, err
		}
		if !result.Completed || result.SessionID != r.baseline.SessionID ||
			result.QuestionHash != string(domain.HashContent([]byte(question.Text()))) || !domain.ValidSessionID(result.MessageID) {
			return result, domain.ErrHashMismatch
		}
		observed := receipt.Clone()
		observed.State, observed.Outcome = "first_turn_observed", "completed"
		observed.Acceptance = "first_turn_completed"
		return result, record(ctx, observed)
	}}, nil
}

func (r nativeClaudeContextReader) inputEstimate(s nativeclaude.ContextSummary) domain.AgentNativeInputEstimate {
	threshold := 0
	known := !s.AutoCompactEnabled
	if s.AutoCompactEnabled && s.AutoCompactThreshold != nil {
		known, threshold = true, int(*s.AutoCompactThreshold)
	}
	return domain.AgentNativeInputEstimate{Provider: domain.ProviderClaude, Model: s.Model, HostVersion: r.host,
		RuntimeScope: r.scope, ContextWindow: int(min(s.MaxTokens, s.RawMaxTokens)),
		AutoCompactKnown: known, AutoCompactTokens: threshold, Measurement: s.Measurement, TotalTokens: int(s.TotalTokens)}
}

// Fixed diagnostics retain errors.Is without disclosing prompts or credentials.
type nativeClaudeContextError struct{ cause error }

func (nativeClaudeContextError) Error() string {
	return "native Claude context preparation or admission failed"
}
func (e nativeClaudeContextError) Unwrap() error { return e.cause }

func (e nativeClaudeContextError) String() string   { return e.Error() }
func (e nativeClaudeContextError) GoString() string { return e.Error() }
func redactNativeClaudeContext(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(nativeClaudeContextError); ok {
		return err
	}
	return nativeClaudeContextError{err}
}
