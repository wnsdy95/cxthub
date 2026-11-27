package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionSummarizer writes a session down at points of its conversation, the
// way a capture's memory is made.
type SessionSummarizer interface {
	// Summarize returns one digest for each end. conversation holds the
	// session's content events from the start, already masked; ends[i] is how
	// many of them make the conversation up to point i. folder is where the
	// session started; the tool's own memory is read for it once, as it is
	// now, and left out when it cannot be read.
	Summarize(ctx context.Context, folder string, provider domain.ProviderKind, sessionID string, conversation domain.CIRDocument, ends []int) ([]domain.MemoryDigest, error)
}

// AnchoredSummaryRemote stores the summary of a session at a commit a
// segment of its report ends at. The server takes the uploader from the
// signed-in account; the report must be stored first.
type AnchoredSummaryRemote interface {
	PutAnchoredSummary(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) error
}

// AnchoredMemoryRemote reads from the server the summaries along a branch,
// the summary of the newest commit first. local names the newest commits of
// the branch as this folder has it, the newest first; the server looks at
// them before what it read of the branch itself.
type AnchoredMemoryRemote interface {
	ReadAnchoredMemory(ctx context.Context, repoID, branch string, local []string, limit int) (domain.AnchoredMemory, error)
}

// BranchSummaryPlace reads from Git where a folder stands.
type BranchSummaryPlace interface {
	// BranchPlace returns the branch the folder is on, empty when it is on
	// none, and the newest commits of that branch, the newest first and at
	// most limit.
	BranchPlace(ctx context.Context, cwd string, limit int) (branch string, newest []string, err error)
}

// BranchSummaryGiven remembers for which branch a session was last given a
// summary. It is a memory of what was given: losing it gives a session the
// summary of its branch once more.
type BranchSummaryGiven interface {
	GivenBranch(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID string) (string, error)
	SetGivenBranch(ctx context.Context, cwd string, provider domain.ProviderKind, sessionID, branch string) error
}
