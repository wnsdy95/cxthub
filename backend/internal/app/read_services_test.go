package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type readServicesSnapshotKey struct{}

type readServicesSnapshotStore struct {
	*store.FSStore
	snapshots     int
	metadataReads int
	finishFailure error
}

func (storage *readServicesSnapshotStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return read(ctx)
	}
	storage.snapshots++
	return errors.Join(read(context.WithValue(ctx, readServicesSnapshotKey{}, storage)), storage.finishFailure)
}

func (storage *readServicesSnapshotStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(readServicesSnapshotKey{}) == storage
}

func (storage *readServicesSnapshotStore) InReadOnlyTransaction(ctx context.Context) bool {
	return storage.InTransaction(ctx)
}

func (storage *readServicesSnapshotStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.metadataReads++
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if !storage.InTransaction(ctx) || permit == nil || !permit.live.Load() || permit.owner != processResources {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

type readServicesRootRequirementStore struct{ *store.FSStore }

func (storage *readServicesRootRequirementStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	metadata, err := storage.FSStore.GetRepo(ctx, repo)
	metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	return metadata, err
}

type readServicesColdDocuments struct {
	*store.FSStore
	readinessChecks int
	demands         int
	wholeBodies     int
	readOnlyReads   int
}

func (storage *readServicesColdDocuments) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (storage *readServicesColdDocuments) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (*readServicesColdDocuments) LegacyByteRangePublicationEnabled() bool { return true }

func (storage *readServicesColdDocuments) HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.readinessChecks++
	return false, nil
}

func (storage *readServicesColdDocuments) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if !outbound.DocReadOnly(ctx) {
		return domain.DocReadIndex{}, domain.ErrForbidden
	}
	storage.readOnlyReads++
	return storage.FSStore.DocReadIndex(ctx, repo, hash)
}

func (storage *readServicesColdDocuments) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.wholeBodies++
	return storage.FSStore.GetDoc(ctx, repo, hash)
}

func (storage *readServicesColdDocuments) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	storage.demands++
	return domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (storage *readServicesColdDocuments) ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error) {
	return domain.DocFindFields{}, domain.ErrDocFindFieldsUnavailable
}

func (storage *readServicesColdDocuments) EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	storage.demands++
	return domain.DocRangeIndexJob{}, domain.ErrIntegrity
}

func (*readServicesColdDocuments) ReadDocDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return domain.DocDisplayStringGeneration{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocEventDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	return domain.DocDisplayProjectionGeneration{}, domain.ErrNotFound
}

func (*readServicesColdDocuments) ReadDocEventDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, domain.ErrNotFound
}

func (storage *readServicesColdDocuments) EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error) {
	storage.demands++
	return domain.DocDisplayJob{}, domain.ErrIntegrity
}
