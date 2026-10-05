package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentInitialPrompt distinguishes an explicitly supplied empty prompt from an
// absent prompt. Text is available only through the deliberate Text accessor.
type AgentInitialPrompt struct {
	text    string
	present bool
}

func NewAgentInitialPrompt(text string) AgentInitialPrompt {
	return AgentInitialPrompt{text: text, present: true}
}

func (p AgentInitialPrompt) Text() string  { return p.text }
func (p AgentInitialPrompt) Present() bool { return p.present }

func (AgentInitialPrompt) String() string   { return "AgentInitialPrompt{redacted}" }
func (AgentInitialPrompt) GoString() string { return "domain.AgentInitialPrompt{redacted}" }

// JSON is deliberately non-authoritative, even when decoded into a used value.
func (AgentInitialPrompt) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

func (p *AgentInitialPrompt) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &struct{}{}); err != nil {
		return err
	}
	*p = AgentInitialPrompt{}
	return nil
}

// AgentPromptReservation binds a prompt to declared accounting for a resolved
// provider/model. A closure keeps prompt bytes out of reflective formatting of
// private fields in enclosing structs, where String and GoString are bypassed.
// The zero value only authorizes a legacy absent prompt with zero tokens.
type AgentPromptReservation struct {
	validate       func(AgentInitialPrompt, ProviderKind, string, string, int) error
	validateBudget func(AgentInitialPrompt, AgentContextBudget) error
}

func NewAgentPromptReservation(prompt AgentInitialPrompt, provider ProviderKind, model string, usage AgentTokenUsage) (AgentPromptReservation, error) {
	return newAgentPromptReservation(prompt, provider, model, "", usage)
}

// NewAgentPromptReservationForCapability binds the actual question to the
// capability-owned policy and runtime scope. It does not confer runtime authority.
// Native allowances count actual UTF-8 bytes, never runes or an estimated codec.
func NewAgentPromptReservationForCapability(prompt AgentInitialPrompt, c AgentHostCapability, usage AgentTokenUsage) (AgentPromptReservation, error) {
	if usage.Tokenizer != c.Tokenizer {
		return AgentPromptReservation{}, ErrProviderCapabilityUnknown
	}
	r, err := newAgentPromptReservation(prompt, c.Provider, c.Model, c.InputAccountingPolicy, usage)
	if err != nil {
		return AgentPromptReservation{}, err
	}
	validate := r.validate
	policy, scope := c.InputAccountingPolicy, c.RuntimeScope
	r.validateBudget = func(actual AgentInitialPrompt, b AgentContextBudget) error {
		if b.InputAccountingPolicy != policy || b.RuntimeScope != scope {
			return fmt.Errorf("%w: initial prompt policy or runtime changed", ErrProviderCapabilityUnknown)
		}
		return validate(actual, b.Provider, b.Model, b.Tokenizer, b.InitialPromptTokens)
	}
	if policy == NativeEstimateReserveV1 {
		if ValidateContentHash(scope) != nil {
			return AgentPromptReservation{}, ErrProviderCapabilityUnknown
		}
		// The legacy API has no policy/scope parameters and cannot authorize an
		// allowance reservation, even when its text and numeric count match.
		r.validate = func(AgentInitialPrompt, ProviderKind, string, string, int) error {
			return ErrProviderCapabilityUnknown
		}
	}
	return r, nil
}

func newAgentPromptReservation(prompt AgentInitialPrompt, provider ProviderKind, model, policy string, usage AgentTokenUsage) (AgentPromptReservation, error) {
	kind, err := ValidateAgentTokenAccounting(policy, provider, usage.Tokenizer, usage)
	if err != nil {
		return AgentPromptReservation{}, err
	}
	if strings.TrimSpace(model) == "" || strings.TrimSpace(usage.Tokenizer) == "" {
		return AgentPromptReservation{}, fmt.Errorf("%w: initial prompt requires a resolved model and accounting", ErrProviderCapabilityUnknown)
	}
	if usage.Tokens < 0 || (prompt.Text() == "" && usage.Tokens != 0) || (prompt.Text() != "" && usage.Tokens == 0) {
		return AgentPromptReservation{}, fmt.Errorf("%w: invalid initial prompt token accounting", ErrContextBudgetExceeded)
	}
	if kind == AgentTextUTF8Allowance && usage.Tokens != len(prompt.Text()) {
		return AgentPromptReservation{}, fmt.Errorf("%w: initial prompt allowance differs from its UTF-8 byte length", ErrContextBudgetExceeded)
	}
	return AgentPromptReservation{validate: func(actual AgentInitialPrompt, actualProvider ProviderKind, actualModel, tokenizer string, tokens int) error {
		if actual != prompt {
			return fmt.Errorf("%w: initial prompt differs from its preparation reservation", ErrContextBudgetExceeded)
		}
		if actualProvider != provider || actualModel != model || tokenizer != usage.Tokenizer {
			return fmt.Errorf("%w: initial prompt reservation does not match the provider, model or tokenizer", ErrProviderCapabilityUnknown)
		}
		if tokens != usage.Tokens {
			return fmt.Errorf("%w: initial prompt count differs from its preparation reservation", ErrContextBudgetExceeded)
		}
		return nil
	}}, nil
}

// ValidateForBudget retains the opaque byte/presence binding and also verifies
// policy/scope for reservations created from a runtime capability.
func (r AgentPromptReservation) ValidateForBudget(prompt AgentInitialPrompt, b AgentContextBudget) error {
	if r.validateBudget != nil {
		return r.validateBudget(prompt, b)
	}
	// A decoded/legacy reservation cannot authorize the new allowance policy.
	if b.InputAccountingPolicy == NativeEstimateReserveV1 {
		return ErrProviderCapabilityUnknown
	}
	return r.Validate(prompt, b.Provider, b.Model, b.Tokenizer, b.InitialPromptTokens)
}

func (r AgentPromptReservation) Validate(prompt AgentInitialPrompt, provider ProviderKind, model, tokenizer string, tokens int) error {
	if r.validate == nil {
		if !prompt.Present() && tokens == 0 {
			return nil
		}
		return fmt.Errorf("%w: initial prompt has no preparation reservation", ErrContextBudgetExceeded)
	}
	return r.validate(prompt, provider, model, tokenizer, tokens)
}

func (AgentPromptReservation) String() string   { return "AgentPromptReservation{redacted}" }
func (AgentPromptReservation) GoString() string { return "domain.AgentPromptReservation{redacted}" }

func (AgentPromptReservation) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

func (r *AgentPromptReservation) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &struct{}{}); err != nil {
		return err
	}
	*r = AgentPromptReservation{}
	return nil
}

// BindInitialPrompt records in-memory authorization without changing the public
// receipt or its digest. Callers validate it again before materialization.
func (p *AgentContextPackage) BindInitialPrompt(res AgentPromptReservation) {
	p.initialPromptReservation = res
}

func (p AgentContextPackage) InitialPromptReservation() AgentPromptReservation {
	return p.initialPromptReservation
}

func (p AgentContextPackage) ValidateInitialPrompt(prompt AgentInitialPrompt) error {
	if p.Budget == nil {
		if p.initialPromptReservation.validate == nil {
			return nil // Legacy memory/artifact paths have no strict prompt budget.
		}
		return fmt.Errorf("%w: initial prompt reservation requires a preparation budget", ErrProviderCapabilityUnknown)
	}
	return p.initialPromptReservation.ValidateForBudget(prompt, *p.Budget)
}

// UnmarshalJSON preserves receipt compatibility but never restores launch
// authorization, including when the destination previously held a binding.
func (p *AgentContextPackage) UnmarshalJSON(data []byte) error {
	type receipt AgentContextPackage
	decoded := receipt(*p)
	decoded.initialPromptReservation = AgentPromptReservation{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = AgentContextPackage(decoded)
	return nil
}
