package domain

import "fmt"

const NativeEstimateReserveV1 = "native_estimate_reserve_v1"
const NativeLocalEstimate = "local_estimate"

// CatalogEstimateReserveV1 uses unverified Codex model-cache window evidence.
// Exact owned-text accounting does not promote that evidence to runtime authority.
const CatalogEstimateReserveV1 = "catalog_estimate_reserve_v1"

// AgentTextAccounting describes owned text, not the assembled native request.
type AgentTextAccounting string

const (
	AgentTextExact         AgentTextAccounting = "exact"
	AgentTextUTF8Allowance AgentTextAccounting = "utf8_byte_allowance"
)

// ValidateAgentTokenAccounting is the shared policy/counter compatibility gate.
// Recognized policies return their classification even on counter mismatch, so
// callers can report the required measurement without repeating policy rules.
// Count/text equality, numeric fit and runtime authority are separate checks.
func ValidateAgentTokenAccounting(policy string, provider ProviderKind, tokenizer string, usage AgentTokenUsage) (AgentTextAccounting, error) {
	var kind AgentTextAccounting
	switch policy {
	case "", MeasuredInputReserveV1:
		kind = AgentTextExact
	case NativeEstimateReserveV1:
		kind = AgentTextUTF8Allowance
	case CatalogEstimateReserveV1:
		kind = AgentTextExact
		if !usage.Exact {
			kind = AgentTextUTF8Allowance
		}
	default:
		return "", fmt.Errorf("%w: unsupported input accounting policy", ErrProviderCapabilityUnknown)
	}
	valid := (provider == ProviderCodex || provider == ProviderClaude) && tokenizer != "" && usage.Tokenizer == tokenizer
	if kind == AgentTextExact {
		valid = valid && usage.Exact
		if policy == CatalogEstimateReserveV1 {
			valid = valid && provider == ProviderCodex && tokenizer != UTF8ByteBoundCounter && (usage.Scope == "" || usage.Scope == "text") && usage.Reason == ""
		}
	} else {
		valid = valid && ((policy == NativeEstimateReserveV1 && provider == ProviderClaude) || (policy == CatalogEstimateReserveV1 && provider == ProviderCodex)) && !usage.Exact && tokenizer == UTF8ByteBoundCounter && usage.Scope == "text" && usage.Reason == "model_tokenizer_unavailable"
	}
	if !valid {
		return kind, fmt.Errorf("%w: text accounting does not match its input policy", ErrProviderCapabilityUnknown)
	}
	return kind, nil
}

// AgentNativeInputEstimate is a fresh whole retained-input summary after the
// reference append, before the real question. Composition must bind the native
// session, reference, question and source. These fields alone are not authority.
type AgentNativeInputEstimate struct {
	Provider          ProviderKind `json:"provider"`
	Model             string       `json:"model"`
	HostVersion       string       `json:"host_version"`
	RuntimeScope      ContentHash  `json:"runtime_scope"`
	ContextWindow     int          `json:"context_window"`
	AutoCompactKnown  bool         `json:"auto_compact_known"`
	AutoCompactTokens int          `json:"auto_compact_tokens"`
	Measurement       string       `json:"measurement"`
	TotalTokens       int          `json:"total_tokens"`
}

// ValidateNativeInputEstimate checks the native estimate plus the still-pending
// question and reserve. Baseline, reference and its framing are already in the
// native total and must not be charged twice. This is local policy, not an exact
// final expanded-input count or provider acceptance.
func (b AgentContextBudget) ValidateNativeInputEstimate(usage AgentTokenUsage, e AgentNativeInputEstimate) error {
	if b.InputAccountingPolicy != NativeEstimateReserveV1 {
		return ErrProviderCapabilityUnknown
	}
	if err := b.Validate(b.Provider, b.Model, b.RequestedTokens, usage); err != nil {
		return err
	}
	if e.Provider != b.Provider || e.Model != b.Model || e.HostVersion != b.HostVersion || e.RuntimeScope != b.RuntimeScope || e.ContextWindow != b.ContextWindow || e.AutoCompactKnown != !b.AutoCompactUnverified || e.AutoCompactTokens != b.AutoCompactTokens || e.Measurement != NativeLocalEstimate || e.TotalTokens < 0 {
		return fmt.Errorf("%w: native input estimate does not match preparation", ErrProviderCapabilityUnknown)
	}
	// Validated budget guarantees both deductions fit without overflow.
	if e.TotalTokens > b.InitialInputLimit-b.InitialPromptTokens-b.OverheadAllowanceTokens {
		return fmt.Errorf("%w: native input estimate exceeds the reserved input limit", ErrContextBudgetExceeded)
	}
	return nil
}
