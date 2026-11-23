package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// ObserveRemotePush acts on a push that the hosting service reported for a Git
// origin: in every repository of that origin the branch ledger learns that the
// branch name is alive or gone, and the remote is read again soon. It returns
// how many repositories that were.
func (s *Service) ObserveRemotePush(ctx context.Context, origin, ref, before, after string, forced bool, delivery string) (int, error) {
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range repos {
		if origin == "" || normalizeGitURL(origin) != normalizeGitURL(r.GitRemoteURL) {
			continue
		}
		observation := domain.GitRefObservation{RepoID: r.ID, GitOrigin: r.GitRemoteURL, Source: "github-push", Ref: ref, Before: before, After: after, Forced: forced, Delivery: delivery}.WithID()
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
