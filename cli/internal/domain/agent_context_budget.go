package domain

import "fmt"

// AgentContextBudget records preparation limits, never provider acceptance.
// Host input is outside the package; its memory, constraints and provenance
// are inside EffectiveTokens. ReservedTokens is required output/work space,
// not another deduction from the already reserved twenty percent.
type AgentContextBudget struct {
	Provider                   ProviderKind `json:"provider"`
	Model                      string       `json:"model"`
	HostVersion                string       `json:"host_version"`
	Tokenizer                  string       `json:"tokenizer"`
	RequestedTokens            int          `json:"requested_tokens"`
	EffectiveTokens            int          `json:"effective_tokens"`
	ContextWindow              int          `json:"context_window"`
	InitialInputLimit          int          `json:"initial_input_limit"`
	HostInputTokens            int          `json:"host_input_tokens"`
	FramingTokens              int          `json:"framing_tokens"`
	InitialPromptTokens        int          `json:"initial_prompt_tokens,omitempty"`
	ReservedTokens             int          `json:"reserved_tokens"`
	AutoCompactTokens          int          `json:"auto_compact_tokens"`
	AdjustmentReason           string       `json:"adjustment_reason,omitempty"`
	InputAccountingPolicy      string       `json:"input_accounting_policy,omitempty"`
	RuntimeScope               ContentHash  `json:"runtime_scope,omitempty"`
	HostInputUnverified        bool         `json:"host_input_unverified,omitempty"`
	AutoCompactUnverified      bool         `json:"auto_compact_unverified,omitempty"`
	OverheadAllowanceTokens    int          `json:"overhead_allowance_tokens,omitempty"`
	ObservedOverheadTokens     int          `json:"observed_overhead_tokens,omitempty"`
	ObservedInputCeilingTokens int          `json:"observed_input_ceiling_tokens,omitempty"`
}

// ResolveBudget must run before body selection. A model name or user-supplied
// window setting cannot replace adapter evidence or exact token accounting.
func (c AgentHostCapability) ResolveBudget(provider ProviderKind, model string, requested int, counter AgentTokenUsage) (AgentContextBudget, error) {
	if (provider != ProviderCodex && provider != ProviderClaude) || !c.Verified || c.Provider != provider || c.Model != model || c.Model == "" || c.HostVersion == "" || c.Evidence == "" || c.ContextWindow <= 0 || c.HostInputTokens < 0 || c.ReservedTokens < 0 || c.FramingTokens < 0 || c.AutoCompactTokens < 0 || !counter.Exact || counter.Tokenizer == "" || counter.Tokenizer != c.Tokenizer {
		return AgentContextBudget{}, fmt.Errorf("%w: verified runtime model/window and exact text tokenizer are required for history", ErrProviderCapabilityUnknown)
	}
	measured := c.InputAccountingPolicy == MeasuredInputReserveV1
	allowance := 0
	if measured {
		scope, err := c.CalibrationScope()
		if err != nil {
			return AgentContextBudget{}, err
		}
		if err := c.Calibration.Validate(scope); err != nil {
			return AgentContextBudget{}, err
		}
		if (!c.HostInputKnown && c.HostInputTokens != 0) || (!c.AutoCompactKnown && c.AutoCompactTokens != 0) {
			return AgentContextBudget{}, fmt.Errorf("%w: unknown runtime input cannot carry a measured count", ErrProviderCapabilityUnknown)
		}
		// Observed overhead already includes known host input and framing.
		// Reserve a fresh margin without subtracting those components twice.
		known := c.HostInputTokens
		if c.FramingTokens > c.ContextWindow || known > c.ContextWindow-c.FramingTokens {
			return AgentContextBudget{}, ErrContextBudgetExceeded
		}
		known += c.FramingTokens
		unexplained := max(0, c.Calibration.OverheadTokens-known)
		margin := InputReserveMargin(c.ContextWindow)
		if unexplained > c.ContextWindow || margin > c.ContextWindow-unexplained {
			return AgentContextBudget{}, ErrContextBudgetExceeded
		}
		allowance = unexplained + margin
	} else if c.InputAccountingPolicy != "" || !c.HostInputKnown || !c.AutoCompactKnown || c.RuntimeScope != "" || c.Calibration != (AgentInputCalibration{}) {
		return AgentContextBudget{}, fmt.Errorf("%w: strict legacy accounting needs known host input and compaction; measured accounting needs an explicit policy", ErrProviderCapabilityUnknown)
	}
	if requested <= 0 || requested > MaxAgentContextTokens || counter.Tokens < 0 || c.InitialPromptTokens < 0 || c.ReservedTokens > c.ContextWindow {
		return AgentContextBudget{}, fmt.Errorf("%w: invalid request or required reservations", ErrContextBudgetExceeded)
	}
	// floor(4*window/5), without overflowing on a large untrusted int.
	limit := c.ContextWindow/5*4 + c.ContextWindow%5*4/5
	reason := "model_window_80_percent"
	if available := c.ContextWindow - c.ReservedTokens; available < limit {
		limit, reason = available, "required_output_reserve"
	}
	// Stay strictly below the known trigger to avoid immediate compaction.
	if c.AutoCompactKnown && c.AutoCompactTokens > 0 && c.AutoCompactTokens-1 < limit {
		limit, reason = c.AutoCompactTokens-1, "automatic_compaction_threshold"
	}
	if measured && c.Calibration.InputCeilingTokens > 0 && c.Calibration.InputCeilingTokens < limit {
		limit, reason = c.Calibration.InputCeilingTokens, "observed_initial_input_limit"
	}
	if c.HostInputTokens >= limit || c.FramingTokens >= limit-c.HostInputTokens {
		return AgentContextBudget{}, fmt.Errorf("%w: mandatory host input leaves no package capacity", ErrContextBudgetExceeded)
	}
	available := limit - c.HostInputTokens - c.FramingTokens
	// Check before subtracting so even hostile counts cannot overflow or leave
	// a non-positive package allowance. The app owns this separate reservation.
	if c.InitialPromptTokens >= available {
		return AgentContextBudget{}, fmt.Errorf("%w: initial prompt leaves no package capacity", ErrContextBudgetExceeded)
	}
	available -= c.InitialPromptTokens
	if allowance >= available {
		return AgentContextBudget{}, fmt.Errorf("%w: internal input allowance leaves no package capacity", ErrContextBudgetExceeded)
	}
	available -= allowance
	effective := min(requested, available)
	if effective == requested {
		reason = ""
	}
	return AgentContextBudget{Provider: provider, Model: model, HostVersion: c.HostVersion, Tokenizer: c.Tokenizer,
		RequestedTokens: requested, EffectiveTokens: effective, ContextWindow: c.ContextWindow, InitialInputLimit: limit,
		HostInputTokens: c.HostInputTokens, FramingTokens: c.FramingTokens, ReservedTokens: c.ReservedTokens,
		InitialPromptTokens: c.InitialPromptTokens,
		AutoCompactTokens:   c.AutoCompactTokens, AdjustmentReason: reason,
		InputAccountingPolicy: c.InputAccountingPolicy, RuntimeScope: c.RuntimeScope,
		HostInputUnverified: measured && !c.HostInputKnown, AutoCompactUnverified: measured && !c.AutoCompactKnown,
		OverheadAllowanceTokens: allowance, ObservedOverheadTokens: c.Calibration.OverheadTokens,
		ObservedInputCeilingTokens: c.Calibration.InputCeilingTokens}, nil
}

// Validate binds a receipt to the request and selected accounting. Recomputing
// limits checks internal consistency; it does not establish host capability.
func (b AgentContextBudget) Validate(provider ProviderKind, model string, requested int, usage AgentTokenUsage) error {
	if usage.Tokens <= 0 {
		return fmt.Errorf("%w: selected package must have positive token accounting", ErrContextBudgetExceeded)
	}
	c := b.capability()
	expected, err := c.ResolveBudget(provider, model, requested, usage)
	if err != nil {
		return err
	}
	if b != expected {
		return fmt.Errorf("%w: preparation budget does not match the request or runtime limits", ErrContextBudgetExceeded)
	}
	if usage.Tokens > b.EffectiveTokens {
		return fmt.Errorf("%w: selected %d tokens; effective package limit %d", ErrContextBudgetExceeded, usage.Tokens, b.EffectiveTokens)
	}
	return nil
}

// Reconstruct accounting for integrity checks, never runtime attestation.
func (b AgentContextBudget) capability() AgentHostCapability {
	c := AgentHostCapability{Provider: b.Provider, Model: b.Model, HostVersion: b.HostVersion, Tokenizer: b.Tokenizer,
		Verified: true, Evidence: "recorded preparation accounting", HostInputKnown: !b.HostInputUnverified, AutoCompactKnown: !b.AutoCompactUnverified,
		ContextWindow: b.ContextWindow, HostInputTokens: b.HostInputTokens, FramingTokens: b.FramingTokens,
		InitialPromptTokens: b.InitialPromptTokens, ReservedTokens: b.ReservedTokens, AutoCompactTokens: b.AutoCompactTokens,
		InputAccountingPolicy: b.InputAccountingPolicy, RuntimeScope: b.RuntimeScope}
	if b.InputAccountingPolicy == MeasuredInputReserveV1 {
		scope, _ := c.CalibrationScope()
		c.Calibration = AgentInputCalibration{Scope: scope, OverheadTokens: b.ObservedOverheadTokens, InputCeilingTokens: b.ObservedInputCeilingTokens}
	}
	return c
}

// EffectiveBudget leaves legacy memory/artifact requests unchanged.
func (p AgentContextPackage) EffectiveBudget() int {
	if p.Budget != nil {
		return p.Budget.EffectiveTokens
	}
	return p.Policy.BudgetTokens
}
