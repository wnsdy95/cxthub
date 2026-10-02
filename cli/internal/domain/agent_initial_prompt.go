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

// AgentPromptReservation binds a prompt to exact accounting for a resolved
// provider/model. A closure keeps prompt bytes out of reflective formatting of
// private fields in enclosing structs, where String and GoString are bypassed.
// The zero value only authorizes a legacy absent prompt with zero tokens.
type AgentPromptReservation struct {
	validate func(AgentInitialPrompt, ProviderKind, string, string, int) error
}

func NewAgentPromptReservation(prompt AgentInitialPrompt, provider ProviderKind, model string, usage AgentTokenUsage) (AgentPromptReservation, error) {
	if (provider != ProviderCodex && provider != ProviderClaude) || strings.TrimSpace(model) == "" || !usage.Exact || strings.TrimSpace(usage.Tokenizer) == "" {
		return AgentPromptReservation{}, fmt.Errorf("%w: initial prompt requires a resolved provider/model and exact tokenizer", ErrProviderCapabilityUnknown)
	}
	if usage.Tokens < 0 || (prompt.Text() == "" && usage.Tokens != 0) || (prompt.Text() != "" && usage.Tokens == 0) {
		return AgentPromptReservation{}, fmt.Errorf("%w: invalid initial prompt token accounting", ErrContextBudgetExceeded)
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
	b := p.Budget
	return p.initialPromptReservation.Validate(prompt, b.Provider, b.Model, b.Tokenizer, b.InitialPromptTokens)
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
