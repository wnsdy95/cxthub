package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// AgentDocumentReader is a verified body cache or lazy server reader. The
// service first authorizes every requested hash through the server projection.
// Implementations must not implicitly fetch/apply refs, settings, or secrets.
type AgentDocumentReader interface {
	GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error)
}

// AgentHistoryPageReader reads complete newest-first turns from the authorized
// archive. The server verifies its chunk/index relationship; a page digest is
// an integrity check on this response, not a proof of the entire document hash.
type AgentHistoryPageReader interface {
	ReadAgentHistoryPage(context.Context, domain.ContentHash, domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error)
}

// PersonalWorkReader returns explicitly recorded work state for the exact
// requested principal/session/worktree. It must not derive approvals from a
// teammate's prose or retrieve an arbitrary latest session for a user.
type PersonalWorkReader interface {
	ReadPersonalWork(context.Context, string, domain.PersonalWorkScope) (domain.PersonalWorkState, error)
}

type AgentTokenCounter interface {
	CountAgentTokens(context.Context, domain.ProviderKind, string, string) (domain.AgentTokenUsage, error)
}

type AgentCapabilityReader interface {
	AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error)
}
