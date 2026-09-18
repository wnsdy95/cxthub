package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, in.RepoID, func(ctx context.Context) (inbound.CommitOutput, error) { return s.commit(ctx, in) })
}
func (s *Service) UpdateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.UpdateRefOutput, error) { return s.updateRefWithPending(ctx, in) })
}
func (s *Service) UpdateRefs(ctx context.Context, in inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	if len(in.Updates) == 0 {
		// Legacy capture clients send empty batches. They can reconcile shared
		// pending pointers, but cannot move a ref or change graph history.
		ctx = pendingWriteContext(ctx)
	}
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.UpdateRefsOutput, error) { return s.updateRefs(ctx, in) })
}
func (s *Service) Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.ForkOutput, error) { return s.fork(ctx, in) })
}
func (s *Service) Join(ctx context.Context, in inbound.JoinInput) (inbound.JoinOutput, error) {
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.JoinOutput, error) { return s.join(ctx, in) })
}

func (s *Service) promoteBoundPR(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, error) {
	return repositoryWrite(ctx, s, repo, func(ctx context.Context) (inbound.UpdateRefOutput, error) {
		return s.promoteBoundPRInTransaction(ctx, repo, pr, baseID)
	})
}
func (s *Service) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	return repositoryWrite(ctx, s, repo, func(ctx context.Context) (domain.PRPromotionJob, error) { return s.submitPRPromotion(ctx, repo, pr) })
}
func (s *Service) PutPending(ctx context.Context, repo domain.ContentHash, session string, p domain.Pending) error {
	return repositoryWriteError(pendingWriteContext(ctx), s, repo, func(ctx context.Context) error { return s.putPending(ctx, repo, session, p) })
}
func (s *Service) GraftSnapshotParents(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, seq uint64) error {
	return repositoryWriteError(ctx, s, repo, func(ctx context.Context) error { return s.graftSnapshotParents(ctx, repo, id, parents, seq) })
}
func (s *Service) GetMemoryProjection(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryProjection, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.MemoryProjection, error) {
		return s.getMemoryProjection(ctx, repo, id)
	})
}
func (s *Service) List(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) ([]domain.Snapshot, error) { return s.list(ctx, in) })
}
func (s *Service) Send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (inbound.PullSendOutput, error) { return s.send(ctx, in) })
}
func (s *Service) GetManifest(ctx context.Context, repo domain.ContentHash) (domain.Manifest, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.Manifest, error) { return s.getManifest(ctx, repo) })
}

func (s *Service) EnsureRepo(ctx context.Context, actor string, repo domain.Repo) (domain.Repo, error) {
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo.ID, func(ctx context.Context) (domain.Repo, error) { return s.ensureRepo(ctx, actor, repo) })
}
func (s *Service) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) ([]domain.Ref, error) { return s.listRefs(ctx, repo) })
}
func (s *Service) Fsck(ctx context.Context, repo domain.ContentHash) (inbound.FsckReport, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (inbound.FsckReport, error) { return s.fsck(ctx, repo) })
}
func (s *Service) GetDoc(ctx context.Context, repo, id domain.ContentHash) (domain.SessionDoc, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.SessionDoc, error) { return s.getDoc(ctx, repo, id) })
}
func (s *Service) GetMemoryDigest(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.MemoryDigest, error) { return s.getMemoryDigest(ctx, repo, id) })
}
func (s *Service) PutUnsync(ctx context.Context, repo domain.ContentHash, user, branch string, u domain.Unsync) error {
	return repositoryWriteError(ctx, s, repo, func(ctx context.Context) error { return s.putUnsync(ctx, repo, user, branch, u) })
}
