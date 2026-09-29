package app

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ConservativeAgentTokenCounter is the shipped memory/artifact counter. It
// counts each UTF-8 byte as one allowance unit. It is deliberately labelled
// inexact, including for ASCII, and can never satisfy strict native history
// launch. Provider-specific framing remains a separate host reservation.
// Replacing this with a tokenizer requires a verified model/tokenizer mapping.
type ConservativeAgentTokenCounter struct{}

func (ConservativeAgentTokenCounter) CountAgentTokens(ctx context.Context, _ domain.ProviderKind, _ string, text string) (domain.AgentTokenUsage, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentTokenUsage{}, err
	}
	return domain.AgentTokenUsage{Tokens: len([]byte(text)), Exact: false, Tokenizer: domain.UTF8ByteBoundCounter}, nil
}
