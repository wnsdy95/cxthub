package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredSessions stores and reads the session reports CLIs upload. A report
// is readable in full by the account that uploaded it; another member reads
// what is open of it: the parts whose commit, or branch, was pushed.
type AnchoredSessions interface {
	PutAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error)
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error)
	AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error)
	// AnchoredSessionCalls lists the recorded agent calls that name one of the
	// caller's sessions as sender or receiver.
	AnchoredSessionCalls(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredAgentCall, error)
	// AnchoredSessionContentAccess says what the caller may read of a
	// session's conversation.
	AnchoredSessionContentAccess(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredContentAccess, error)
	// SetAnchoredSessionArchived archives a session or restores it: its
	// uploader may, and whoever manages the repository.
	SetAnchoredSessionArchived(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, archived bool) (domain.AnchoredSessionArchive, error)
	// RemoveAnchoredSession removes a session and the sessions it spawned
	// from the server and remembers the removal: its uploader may, and
	// whoever manages the repository.
	RemoveAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionRemovalReceipt, error)
	// AllowAnchoredSession takes back a removal, so that the session is
	// taken again when a CLI uploads it.
	AllowAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error
	// ListAnchoredSessionRemovals lists the caller's own removed sessions.
	ListAnchoredSessionRemovals(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionRemovalView, error)
}

// AnchoredSearch looks for a query in what the sessions tab shows: pull
// requests, branches, commits, conversations and notes, as far as the caller
// may read them.
type AnchoredSearch interface {
	SearchAnchored(ctx context.Context, repo domain.ContentHash, query string) (domain.AnchoredSearchResult, error)
}

// AnchoredSummaries stores and reads the summary of a session at a commit a
// segment of it ends at. A summary is read under the rule of that segment.
type AnchoredSummaries interface {
	PutAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) (domain.AnchoredSummaryReceipt, error)
	ListAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error)
	// ReadAnchoredMemory lists the summaries along a branch, the summary of
	// the newest commit first. local names the newest commits of the branch
	// as the caller's folder has it; they are looked at first.
	ReadAnchoredMemory(ctx context.Context, repo domain.ContentHash, branch string, local []string, limit int) (domain.AnchoredMemory, error)
}

// AnchoredQueries is the read side of the session structure, for a process
// that only reads: sessions, the segments at a commit, summaries at commits
// and along a branch, the search, and a session's conversation in pieces a
// tool takes at once. Every read is answered under the rule of the reader in
// the context: all of a session to its uploader, the open parts to anyone
// else.
type AnchoredQueries interface {
	GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error)
	ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error)
	AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error)
	ListAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error)
	ReadAnchoredMemory(ctx context.Context, repo domain.ContentHash, branch string, local []string, limit int) (domain.AnchoredMemory, error)
	SearchAnchored(ctx context.Context, repo domain.ContentHash, query string) (domain.AnchoredSearchResult, error)
	// ReadAnchoredSessionFragments reads a session's conversation from one
	// event on, within a byte budget and within what the caller may read.
	ReadAnchoredSessionFragments(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, index, offset, limit, budget int) (domain.AnchoredFragmentPage, error)
}
