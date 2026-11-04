package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	commands, err := s.documentPublications(DocumentPublicationCompatibility)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	return commands.Commit(ctx, in)
}
func (s *Service) UpdateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	return s.repositoryRefs().UpdateRef(ctx, in)
}
func (s *Service) UpdateRefs(ctx context.Context, in inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	return s.repositoryRefs().UpdateRefs(ctx, in)
}
func (s *Service) Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	return s.branchOperations().Fork(ctx, in)
}

func (s *Service) promoteBoundPR(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, error) {
	return s.prPromotion().promoteBoundPR(ctx, repo, pr, baseID)
}
func (s *Service) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	return s.prCoordinations().SubmitPRPromotion(ctx, repo, pr)
}
func (s *Service) PutPending(ctx context.Context, repo domain.ContentHash, session string, p domain.Pending) error {
	ctx = auditOperation(ctx, "context.pending.updated")
	return repositoryWriteError(pendingWriteContext(ctx), s, repo, func(ctx context.Context) error { return s.putPending(ctx, repo, session, p) })
}
func (s *Service) GraftSnapshotParents(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, seq uint64) error {
	commands, err := snapshotGraftsForWork(s.meta, s.repositoryWork())
	if err != nil {
		return err
	}
	return commands.GraftSnapshotParents(ctx, repo, id, parents, seq)
}
func (s *Service) GetMemoryProjection(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryProjection, error) {
	queries, err := s.MemorySnapshotQueries()
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return queries.GetMemoryProjection(ctx, repo, id)
}
func (s *Service) List(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
	return repositoryReadForRepo(ctx, s, in.RepoID, func(ctx context.Context) ([]domain.Snapshot, error) { return s.list(ctx, in) })
}
func (s *Service) Send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error) {
	return resourceReadForRepo(ctx, s, in.RepoID, func(ctx context.Context) (inbound.PullSendOutput, error) { return s.send(ctx, in) })
}
func (s *Service) GetManifest(ctx context.Context, repo domain.ContentHash) (domain.Manifest, error) {
	return repositoryReadForRepo(ctx, s, repo, func(ctx context.Context) (domain.Manifest, error) { return s.getManifest(ctx, repo) })
}

func (s *Service) EnsureRepo(ctx context.Context, actor string, repo domain.Repo) (domain.Repo, error) {
	if repo.RequiredDocIdentity != domain.DocumentIdentityLegacy {
		return domain.Repo{}, domain.ErrValidation
	}
	ctx = writeAction(inbound.WithRepositoryActor(ctx, actor), "register")
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo.ID, func(ctx context.Context) (domain.Repo, error) { return s.ensureRepo(ctx, actor, repo) })
}
func (s *Service) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return repositoryReadForRepo(ctx, s, repo, func(ctx context.Context) ([]domain.Ref, error) { return s.listRefs(ctx, repo) })
}
func (s *Service) Fsck(ctx context.Context, repo domain.ContentHash) (inbound.FsckReport, error) {
	return resourceReadForRepo(ctx, s, repo, func(ctx context.Context) (inbound.FsckReport, error) { return s.fsck(ctx, repo) })
}
func (s *Service) GetDoc(ctx context.Context, repo, id domain.ContentHash) (domain.SessionDoc, error) {
	return resourceReadForRepo(ctx, s, repo, func(ctx context.Context) (domain.SessionDoc, error) { return s.getDoc(ctx, repo, id) })
}

// GetMemoryDigest resolves the authoritative MemoryHash pointer. Metadata-only
// fallback is intentionally limited to legacy snapshots with no pointer: once
// a pointer exists, hiding a missing/corrupt blob behind stale metadata would
// turn an integrity failure into a silent rollback.
func (s *Service) GetMemoryDigest(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	queries, err := s.MemorySnapshotQueries()
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	return queries.GetMemoryDigest(ctx, repo, id)
}
func (s *Service) PutUnsync(ctx context.Context, repo domain.ContentHash, user, branch string, u domain.Unsync) error {
	ctx = auditOperation(ctx, "context.unsync.updated")
	ctx = context.WithValue(ctx, revisionScopeKey{}, "none")
	return repositoryWriteError(ctx, s, repo, func(ctx context.Context) error { return s.putUnsync(ctx, repo, user, branch, u) })
}

func (s *Service) StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	commands, err := s.documentPublications(DocumentPublicationCompatibility)
	if err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	return commands.StoreChunks(ctx, in)
}
func (s *Service) PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error {
	return repositoryWriteError(ctx, s, repoID, func(ctx context.Context) error { return s.promoteSnapshotMessageCommand(ctx, repoID, id, message) })
}
func (service *Service) UpdateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.UpdateAbout(ctx, repo, description, website, topics)
}
func (service *Service) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.UpdateRepoConfig(ctx, repo, branch, protected)
}
func (service *Service) PutSettings(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.PutSettings(ctx, repo, bundle)
}
func (service *Service) PutSettingsObject(ctx context.Context, repo, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.PutSettingsObject(ctx, repo, hash, bundle)
}
func (service *Service) PutMemoryDigest(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	commands, err := memoryDigestsForWork(service.blobs, service.meta, service.repositoryWork())
	if err != nil {
		return "", err
	}
	return commands.PutMemoryDigest(ctx, repo, digest)
}
func (service *Service) PutMemoryDigestCAS(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	commands, err := memoryDigestsForWork(service.blobs, service.meta, service.repositoryWork())
	if err != nil {
		return "", err
	}
	return commands.PutMemoryDigestCAS(ctx, repo, digest)
}
