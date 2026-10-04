package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

const agentBoundedProjectionGap = "bounded_projection_remaining_sources_via_mcp"

// AgentTokenMeasurementError means a candidate could not be certified, not that
// an exact token count exceeded its budget. Reason is a bounded diagnostic code;
// the error never contains the candidate's text. Native history requires exact
// accounting even when a conservative allowance fits.
type AgentTokenMeasurementError struct {
	Reason        string
	Allowance     int
	Budget        int
	ExactRequired bool
}

func (e *AgentTokenMeasurementError) Error() string {
	message := fmt.Sprintf("cannot certify package token fit (%s): conservative allowance %d, effective budget %d; exact token count is unavailable", e.Reason, e.Allowance, e.Budget)
	if e.ExactRequired {
		message += "; native history requires exact token accounting"
	}
	return message
}

func (*AgentTokenMeasurementError) Unwrap() error { return domain.ErrProviderCapabilityUnknown }

func agentTokenMeasurementFailure(usage domain.AgentTokenUsage, budget int, exactRequired bool) error {
	reason := usage.Reason
	switch reason {
	case "tokenizer_work_limit", "unicode_classification_unavailable", "model_tokenizer_unavailable":
	default:
		reason = "token_measurement_unavailable"
	}
	return &AgentTokenMeasurementError{Reason: reason, Allowance: usage.Tokens, Budget: budget, ExactRequired: exactRequired}
}

func agentCandidateLimit(err error) bool {
	var measurement *AgentTokenMeasurementError
	return errors.Is(err, domain.ErrContextBudgetExceeded) || errors.As(err, &measurement)
}

func markAgentMeasurementGap(p *domain.AgentContextPackage, scope string, err error) {
	var measurement *AgentTokenMeasurementError
	if !errors.As(err, &measurement) {
		return
	}
	gap := domain.AgentCoverageGap{Reason: scope + "_" + measurement.Reason}
	p.Content.Gaps = append([]domain.AgentCoverageGap(nil), p.Content.Gaps...)
	for i := range p.Content.Gaps {
		if p.Content.Gaps[i].Reason == agentBoundedProjectionGap {
			// Reuse the general notice, but do not assume that the replacement
			// has a smaller token count. History selection measures it again.
			p.Content.Gaps[i] = gap
			return
		}
	}
	p.Content.Gaps = append(p.Content.Gaps, gap)
}

func (s *AgentContextService) finishAgentHistorySelection(ctx context.Context, in inbound.PrepareAgentContextInput, p *domain.AgentContextPackage, rejected error) error {
	var measurement *AgentTokenMeasurementError
	if !errors.As(rejected, &measurement) {
		return nil
	}
	markAgentMeasurementGap(p, "history", rejected)
	_, err := s.measure(ctx, in, *p)
	if err == nil || !agentCandidateLimit(err) {
		return err
	}
	// The factual diagnostic is part of every candidate in this second search.
	// It can add tokens even when replacing a longer notice, or when the memory
	// diagnostic already consumed that notice. Drop only oldest complete turns,
	// including turns from prior pages, until the final rendered package fits.
	history := p.Content.History
	accepted := 0
	rejected = err
	for low, high := 1, len(history)-1; low <= high; {
		middle := low + (high-low)/2
		candidate := *p
		candidate.Content.History = history[len(history)-middle:]
		_, err := s.measure(ctx, in, candidate)
		if err == nil {
			accepted = middle
			low = middle + 1
		} else if agentCandidateLimit(err) {
			rejected = err
			high = middle - 1
		} else {
			return err
		}
	}
	if accepted == 0 {
		return fmt.Errorf("newest complete turn cannot be selected: %w", rejected)
	}
	p.Content.History = history[len(history)-accepted:]
	return nil
}

// ConservativeAgentTokenCounter is the fallback memory/artifact counter. It
// counts each UTF-8 byte as one allowance unit. It is deliberately labelled
// inexact, including for ASCII, and can never satisfy strict native history
// launch. Provider-specific framing remains a separate host reservation.
// The runtime prefers the agenttokens adapter for documented model mappings.
type ConservativeAgentTokenCounter struct{}

func (ConservativeAgentTokenCounter) CountAgentTokens(ctx context.Context, _ domain.ProviderKind, _ string, text string) (domain.AgentTokenUsage, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	return domain.AgentTokenUsage{Tokens: len([]byte(text)), Exact: false, Tokenizer: domain.UTF8ByteBoundCounter}, nil
}
