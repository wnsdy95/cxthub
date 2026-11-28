package app

// The repository webhook that told the service of a push or of a change of a
// pull request is gone: GitHub tells through the GitHub App, whose worker
// records a push and asks for a read itself (GitHubWorker.applyDelivery).
// Tests still tell the service of a push through ObserveRemotePush, with the
// same two steps (recordBranchLedgerObservation and requestRemoteGitSync) and,
// like the worker, as a write of evidence. It is not the App's path: it chooses
// repositories by origin, where the worker goes by binding. A test of what a
// delivery does goes through GitHubConnections.Receive and the worker, as
// TestBranchLedgerFollowsQualifiedPushDeliveries and, for a pull request,
// TestRemoteGitReaderIsToldOfADeliveredPullRequest do.

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// ObserveRemotePush acts on a push that the hosting service reported for a Git
// origin: in every repository of that origin the branch ledger learns that the
// branch name is alive or gone, and the remote is read again soon. It returns
// how many repositories that were.
func (s *Service) ObserveRemotePush(ctx context.Context, origin, ref, after, delivery string) (int, error) {
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range repos {
		if origin == "" || normalizeGitURL(origin) != normalizeGitURL(r.GitRemoteURL) {
			continue
		}
		// The push stands for one the GitHub App delivered, the only kind the
		// ledger takes.
		observation := domain.GitRefObservation{RepoID: r.ID, GitOrigin: r.GitRemoteURL, Source: "github-push", Ref: ref, After: after, Delivery: delivery, Provider: &domain.GitProviderObservation{}}.WithID()
		err = repositoryWriteError(evidenceWriteContext(ctx), s, r.ID, func(tx context.Context) error {
			current, err := s.meta.GetRepo(tx, r.ID)
			if err != nil {
				return err
			}
			if current.GitRemoteURL != r.GitRemoteURL {
				return domain.ErrConflict
			}
			observed := time.Now().UTC()
			if err := recordBranchLedgerObservation(tx, s.meta, observation, observed); err != nil {
				return err
			}
			return requestRemoteGitSync(tx, s.meta, r.ID, observed)
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
