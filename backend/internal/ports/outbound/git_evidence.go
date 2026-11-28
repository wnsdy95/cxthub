package outbound

import (
	"errors"
	"fmt"
)

// ErrGitCommitUnavailable means the provider served the repository but not the
// requested commit: it was never pushed, or it was rewritten locally before it
// was pushed. Retrying the same read cannot succeed until provider evidence
// (a push, a branch head, or a descendant) names the commit.
var ErrGitCommitUnavailable = errors.New("Git commit is unavailable at the provider")

// ErrGitRepositoryNotConnected means the server may not read the repository:
// no installation of the GitHub App was told to read it. It is a lasting
// state, not a failure of the provider, and ends when a maintainer connects
// the repository.
var ErrGitRepositoryNotConnected = errors.New("the repository is not connected to the Git hosting provider")

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
