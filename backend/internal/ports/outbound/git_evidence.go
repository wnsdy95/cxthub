package outbound

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// GitEvidenceReader reads immutable commit/tree objects from a configured Git
// host. Repository origin comes from trusted repository metadata, never a caller
// supplied URL. A missing/ambiguous comparison parent must fail explicitly.
type GitEvidenceReader interface {
	ReadCommitDelta(context.Context, string, string, string) (domain.GitCommitDelta, error)
	IsGitAncestor(context.Context, string, string, string) (bool, error)
}

// ErrGitCommitUnavailable means the provider served the repository but not the
// requested commit: it was never pushed, or it was rewritten locally before it
// was pushed. Retrying the same read cannot succeed until provider evidence
// (a push, a branch head, or a descendant) names the commit.
var ErrGitCommitUnavailable = errors.New("Git commit is unavailable at the provider")

// ErrGitProviderRateLimited reports a read refused locally while a provider
// rate-limit cooldown is in effect.
var ErrGitProviderRateLimited = errors.New("Git evidence provider rate limit; retry later")

// GitProviderStatusError carries only a provider HTTP status. Bodies, URLs and
// credentials stay out of it so workers can log a bounded failure class.
type GitProviderStatusError struct {
	Status int
}

func (failure GitProviderStatusError) Error() string {
	return fmt.Sprintf("Git evidence provider returned HTTP %d", failure.Status)
}
