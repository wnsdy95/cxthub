package main

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// The native adapter owns policy provenance; this composition translates units
// into the domain's 80% packing policy. Nothing here attests remote acceptance.
// Invocation-scoped calibration avoids reading/copying native credentials.
type nativeCodexWindowReader struct {
	session *nativecodex.Session
	binding nativecodex.WindowPolicy
	thread  nativecodex.Thread
	host    string
	counter outbound.AgentTokenCounter
}

var _ outbound.AgentCapabilityReader = nativeCodexWindowReader{}

func (nativeCodexWindowReader) String() string     { return "native Codex window reader (private input)" }
func (r nativeCodexWindowReader) GoString() string { return r.String() }
func (r nativeCodexWindowReader) AgentCapability(ctx context.Context, provider domain.ProviderKind, model string) (domain.AgentHostCapability, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentHostCapability{}, err
	}
	if provider != domain.ProviderCodex || model != r.thread.Model || r.counter == nil || r.host == "" || r.binding == nil {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	if err := r.binding.Validate(ctx, r.thread); err != nil {
		return domain.AgentHostCapability{}, fmt.Errorf("%w: native runtime binding changed", domain.ErrProviderCapabilityUnknown)
	}
	window := r.binding.ModelWindow()
	if window.Model != model || window.ModelProvider != r.thread.ModelProvider || window.PackingUsableWindow <= 0 || window.PackingUsableWindow > int64(^uint(0)>>1) || window.NativeUsableWindow < window.PackingUsableWindow {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	usage, err := r.counter.CountAgentTokens(ctx, provider, model, "")
	if err != nil {
		return domain.AgentHostCapability{}, err
	}
	c := domain.AgentHostCapability{
		Provider: provider, Model: model, HostVersion: r.host, Verified: true,
		Evidence:      "native 0.157.1 existing static catalog; startup and pre-injection drift checks; provider acceptance unverified",
		ContextWindow: int(window.PackingUsableWindow), Tokenizer: usage.Tokenizer,
		RuntimeScope:          domain.ContentHash(r.binding.RuntimeScope()),
		InputAccountingPolicy: domain.MeasuredInputReserveV1,
	}
	if estimate, ok := r.binding.(nativecodex.WindowEstimate); ok {
		c.Verified = false
		c.Evidence = estimate.Evidence()
		c.InputAccountingPolicy = domain.CatalogEstimateReserveV1
		c.WindowEstimateSource = estimate.Source()
		c.WindowEstimateObservedAt = estimate.ObservedAt()
		c.WindowEstimateHash = domain.ContentHash(estimate.CatalogHash())
		c.WindowEstimateClientVersion = estimate.CatalogClientVersion()
	}
	if _, err := domain.ValidateAgentTokenAccounting(c.InputAccountingPolicy, provider, c.Tokenizer, usage); err != nil {
		return domain.AgentHostCapability{}, err
	}
	if window.AutoCompactTokenLimitScope == "total" && window.AutoCompactTokenLimit > 0 && window.AutoCompactTokenLimit <= int64(^uint(0)>>1) {
		c.AutoCompactKnown = true
		c.AutoCompactTokens = int(window.AutoCompactTokenLimit)
	}
	if c.InputAccountingPolicy == domain.CatalogEstimateReserveV1 {
		return c, nil
	}
	scope, err := c.CalibrationScope()
	if err != nil {
		return domain.AgentHostCapability{}, err
	}
	c.Calibration = domain.AgentInputCalibration{Scope: scope}
	return c, nil
}

// startWithWindow preserves the same parsed native arguments as inspection.
// Static catalogs retain their binding; stock dynamic catalogs supply an
// explicitly estimated policy. Neither path changes native configuration.
func (n nativeCodexLaunch) startWithWindow(ctx context.Context, env []string) (*nativecodex.Session, nativecodex.Thread, nativeCodexWindowReader, error) {
	opts := n.process
	opts.ConfigArgs = append([]string(nil), opts.ConfigArgs...)
	if env != nil {
		opts.Env = append([]string{}, env...)
	}
	session, thread, binding, err := nativecodex.StartWindowPolicy(ctx, opts, n.thread)
	if err != nil {
		return nil, nativecodex.Thread{}, nativeCodexWindowReader{}, err
	}
	reader := nativeCodexWindowReader{session: session, binding: binding, thread: thread, host: session.HostIdentity(), counter: runtimeAgentTokens}
	if _, err = reader.AgentCapability(ctx, domain.ProviderCodex, thread.Model); err != nil {
		_ = session.Close()
		return nil, nativecodex.Thread{}, nativeCodexWindowReader{}, err
	}
	return session, thread, reader, nil
}

// Derive preparation, revalidation, and telemetry units from one owned binding.
// Callers cannot accidentally pair a smaller policy budget with another thread's
// native telemetry comparator while composing the delayed first-turn route.
func newNativeCodexWindowGenerationPrepare(bound nativeCodexLaunch, session *nativecodex.Session, reader nativeCodexWindowReader,
	prepare nativeCodexPackagePrepare, validate nativeCodexPackageValidate, store outbound.AgentInputCalibrationStore,
) (func(context.Context, nativecodex.Thread, string) (nativecodex.PreparedGeneration, error), error) {
	if session == nil || reader.session != session || reader.binding == nil || prepare == nil || validate == nil {
		return nil, domain.ErrProviderCapabilityUnknown
	}
	window := reader.binding.ModelWindow()
	if window.NativeUsableWindow <= 0 || window.NativeUsableWindow > int64(^uint(0)>>1) {
		return nil, domain.ErrProviderCapabilityUnknown
	}
	check := func(ctx context.Context, p domain.AgentContextPackage) error {
		c, err := reader.AgentCapability(ctx, domain.ProviderCodex, reader.thread.Model)
		if err != nil {
			return err
		}
		return checkNativeWindowPackage(c, p)
	}
	prepareBound := func(ctx context.Context, thread nativecodex.Thread, prompt domain.AgentInitialPrompt) (domain.AgentContextPackage, error) {
		if thread != reader.thread {
			return domain.AgentContextPackage{}, domain.ErrProviderCapabilityUnknown
		}
		if _, err := reader.AgentCapability(ctx, domain.ProviderCodex, thread.Model); err != nil {
			return domain.AgentContextPackage{}, err
		}
		p, err := prepare(ctx, thread, prompt)
		if err != nil {
			return p, err
		}
		return p, check(ctx, p)
	}
	validateBound := func(ctx context.Context, p domain.AgentContextPackage) error {
		if err := check(ctx, p); err != nil {
			return err
		}
		return validate(ctx, p)
	}
	return newNativeCodexGenerationPrepare(bound, session, reader.thread, reader.host, int(window.NativeUsableWindow), prepareBound, validateBound, store)
}
func checkNativeWindowPackage(c domain.AgentHostCapability, p domain.AgentContextPackage) error {
	b := p.Budget
	if b == nil || b.Provider != c.Provider || b.Model != c.Model || b.HostVersion != c.HostVersion ||
		b.ContextWindow != c.ContextWindow || b.Tokenizer != c.Tokenizer || b.RuntimeScope != c.RuntimeScope ||
		b.AutoCompactTokens != c.AutoCompactTokens || b.AutoCompactUnverified == c.AutoCompactKnown ||
		b.InputAccountingPolicy != c.InputAccountingPolicy ||
		b.WindowEstimateSource != c.WindowEstimateSource || b.WindowEstimateObservedAt != c.WindowEstimateObservedAt || b.WindowEstimateHash != c.WindowEstimateHash || b.WindowEstimateClientVersion != c.WindowEstimateClientVersion {
		return domain.ErrProviderCapabilityUnknown
	}
	c.InitialPromptTokens = b.InitialPromptTokens
	_, err := c.ResolveBudget(p.Provider, b.Model, p.Policy.BudgetTokens, p.Usage)
	return err
}
