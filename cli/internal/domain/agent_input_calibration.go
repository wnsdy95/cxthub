package domain

import (
	"encoding/json"
	"fmt"
)

const MeasuredInputReserveV1 = "measured_reserve_v1"

// AgentInputCalibration contains no prompt, credentials or provider output.
// Max overhead / min ceiling make duplicate and reordered observations safe.
// Zero ceiling means no observed restriction, not unlimited model capacity.
type AgentInputCalibration struct {
	Scope              ContentHash `json:"scope"`
	OverheadTokens     int         `json:"overhead_tokens"`
	InputCeilingTokens int         `json:"input_ceiling_tokens"`
}

func (c AgentInputCalibration) Validate(scope ContentHash) error {
	if ValidateContentHash(scope) != nil || c.Scope != scope || c.OverheadTokens < 0 || c.InputCeilingTokens < 0 {
		return fmt.Errorf("%w: invalid input calibration", ErrProviderCapabilityUnknown)
	}
	return nil
}

func (c AgentInputCalibration) Merge(other AgentInputCalibration) (AgentInputCalibration, error) {
	if err := c.Validate(other.Scope); err != nil {
		return AgentInputCalibration{}, err
	}
	if err := other.Validate(c.Scope); err != nil {
		return AgentInputCalibration{}, err
	}
	c.OverheadTokens = max(c.OverheadTokens, other.OverheadTokens)
	if other.InputCeilingTokens > 0 && (c.InputCeilingTokens == 0 || other.InputCeilingTokens < c.InputCeilingTokens) {
		c.InputCeilingTokens = other.InputCeilingTokens
	}
	return c, nil
}

// CalibrationScope binds feedback to runtime evidence, not a model name alone.
// The adapter must change RuntimeScope when routing, account or effective
// instruction/tool settings change. The hash is not a capacity attestation.
func (c AgentHostCapability) CalibrationScope() (ContentHash, error) {
	if !c.Verified || c.Evidence == "" || (c.Provider != ProviderCodex && c.Provider != ProviderClaude) || c.Model == "" || c.HostVersion == "" || c.Tokenizer == "" || c.ContextWindow <= 0 || ValidateContentHash(c.RuntimeScope) != nil {
		return "", fmt.Errorf("%w: runtime scope and model window evidence are required", ErrProviderCapabilityUnknown)
	}
	raw, _ := json.Marshal(struct {
		Policy, Provider, Model, Host, Tokenizer string
		Window                                   int
		Runtime                                  ContentHash
	}{MeasuredInputReserveV1, string(c.Provider), c.Model, c.HostVersion, c.Tokenizer, c.ContextWindow, c.RuntimeScope})
	return HashContent(raw), nil
}

// InputReserveMargin is a conservative engineering allowance, not an exact
// upper bound on hidden input. Reserve it inside the 80% input limit.
func InputReserveMargin(window int) int { return max(16000, window/20) }

// AgentInputObservation is supplied only by a correlated native-turn adapter.
// TotalInputTokens must include cached input; a missing usage report is unknown.
// Only the first request's pre-compaction usage can calibrate initial input.
type AgentInputObservation struct {
	Scope                 ContentHash
	PackageID             ContentHash
	Outcome               string // completed, input_rejected, initial_compaction, unknown
	InitialRequest        bool
	ExecutionKnown        bool
	ExecutionStarted      bool
	UsageKnown            bool
	UsageBeforeCompaction bool
	TotalInputTokens      int
	SubmittedTextTokens   int
	SubmittedTextExact    bool
	RetryAttempt          int
}

// ObserveInput never starts or retries a model call. It derives next-preparation
// feedback and permits at most one retry only for proven pre-execution input
// rejection. A timeout, disconnect, completed turn or compaction cannot retry.
func (b AgentContextBudget) ObserveInput(packageID ContentHash, selected AgentTokenUsage, o AgentInputObservation) (AgentInputCalibration, bool, error) {
	if err := b.Validate(b.Provider, b.Model, b.RequestedTokens, selected); err != nil {
		return AgentInputCalibration{}, false, err
	}
	c := b.capability()
	scope, err := c.CalibrationScope()
	if err != nil || b.InputAccountingPolicy != MeasuredInputReserveV1 || ValidateContentHash(packageID) != nil || o.PackageID != packageID || o.Scope != scope || !o.InitialRequest || o.RetryAttempt < 0 || o.TotalInputTokens < 0 || o.SubmittedTextTokens < 0 || (o.ExecutionStarted && !o.ExecutionKnown) {
		return AgentInputCalibration{}, false, fmt.Errorf("%w: observation does not match prepared input", ErrProviderCapabilityUnknown)
	}
	// Supplied local submission evidence must agree even when native usage is
	// absent. An omitted execution flag likewise cannot attest non-execution.
	if (o.SubmittedTextExact && o.SubmittedTextTokens != selected.Tokens+b.InitialPromptTokens) || (!o.SubmittedTextExact && o.SubmittedTextTokens != 0) {
		return AgentInputCalibration{}, false, fmt.Errorf("%w: submitted text accounting does not match preparation", ErrProviderCapabilityUnknown)
	}
	feedback := c.Calibration
	if o.UsageKnown {
		// Sum is safe: Validate bounded both components below the input limit.
		if !o.SubmittedTextExact || !o.UsageBeforeCompaction || o.SubmittedTextTokens != selected.Tokens+b.InitialPromptTokens || o.TotalInputTokens < o.SubmittedTextTokens {
			return AgentInputCalibration{}, false, fmt.Errorf("%w: incomplete initial input usage", ErrProviderCapabilityUnknown)
		}
		feedback.OverheadTokens = max(feedback.OverheadTokens, o.TotalInputTokens-o.SubmittedTextTokens)
	} else if o.TotalInputTokens != 0 || o.UsageBeforeCompaction {
		return AgentInputCalibration{}, false, fmt.Errorf("%w: unknown input usage has a count", ErrProviderCapabilityUnknown)
	}
	switch o.Outcome {
	case "completed", "unknown":
		return feedback, false, nil
	case "input_rejected":
		if o.ExecutionStarted {
			return AgentInputCalibration{}, false, fmt.Errorf("%w: rejection followed execution", ErrProviderCapabilityUnknown)
		}
	case "initial_compaction":
	default:
		return AgentInputCalibration{}, false, fmt.Errorf("%w: unknown observation outcome", ErrProviderCapabilityUnknown)
	}
	// A small package may have failed even though the request allowed much more.
	// Reduce from the submitted estimated size, not that unused request capacity.
	base := selected.Tokens + b.InitialPromptTokens
	for _, n := range []int{b.HostInputTokens, b.FramingTokens, b.OverheadAllowanceTokens} {
		base += n // validated sum stays <= InitialInputLimit
	}
	if o.UsageKnown {
		base = min(base, o.TotalInputTokens)
	}
	ceiling := max(1, base-InputReserveMargin(b.ContextWindow))
	if feedback.InputCeilingTokens == 0 || ceiling < feedback.InputCeilingTokens {
		feedback.InputCeilingTokens = ceiling
	}
	return feedback, o.Outcome == "input_rejected" && o.ExecutionKnown && o.RetryAttempt == 0, nil
}
