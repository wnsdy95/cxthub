package documentquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var errDocViewGenerationNeedsPreparation = fmt.Errorf("%w: document generation is not ready", domain.ErrDocRangeIndexUnavailable)
var errDocViewBoundaryNeedsPreparation = fmt.Errorf("%w: inherited prefix is not ready", domain.ErrDocRangeIndexUnavailable)

type docRangeRequirement uint8

const (
	docRangeRootOnly docRangeRequirement = iota
	docRangeLegacyMetadata
	docRangeLegacyBytes
)

func (queries *Queries) docBytePreparation() (DocByteRangeReadiness, bool) {
	storage := queries.view.ByteReadiness
	if storage == nil {
		storage = queries.view.Bytes
	}
	return storage, storage != nil && storage.LegacyByteRangePublicationEnabled()
}

func (queries *Queries) prepareDocViewSource(ctx context.Context, repo, hash domain.ContentHash, requirement docRangeRequirement) (bool, error) {
	ctx = queries.readContext(ctx)
	if err := domain.ValidateContentHash(hash); err != nil {
		return false, err
	}
	var prepared, needs bool
	bytes, byteEnabled := queries.docBytePreparation()
	requireBytes := false
	err := queries.interactiveViewWork(ctx, func(admitted context.Context) error {
		var err error
		admitted, err = queries.reauthorizeDocumentRead(admitted, repo)
		if err != nil {
			return err
		}
		needs, err = repositoryReadForRepo(admitted, queries, repo, func(read context.Context) (bool, error) {
			snapshot, err := queries.metadata.GetSnapshot(read, repo, hash)
			if errors.Is(err, domain.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
				return false, err
			}
			if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
				if requirement == docRangeRootOnly {
					return false, nil
				}
				generations := queries.view.RangeGenerations
				if generations == nil || !queries.usesLegacyRangeReadiness() {
					return false, nil
				}
				if !generations.InReadOnlyTransaction(read) {
					return false, domain.ErrConflict
				}
				readiness := queries.view.RangeReadiness
				if readiness == nil {
					return false, domain.ErrDocRangeIndexUnavailable
				}
				prepared = true
				requireBytes = requirement == docRangeLegacyBytes && byteEnabled
				if requireBytes {
					ready, err := bytes.HasDocByteRangeIndex(read, repo, hash)
					return !ready, err
				}
				ready, err := readiness.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			prepared = true
			storage := queries.view.Range
			if storage == nil {
				return false, domain.ErrDocRangeIndexUnavailable
			}
			if readiness := queries.view.RangeReadiness; readiness != nil {
				ready, err := readiness.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			_, err = storage.DocRangeIndex(read, repo, hash)
			if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				return true, nil
			}
			return false, err
		})
		return err
	})
	if err != nil || !needs {
		return prepared, err
	}
	if outbound.DocReadOnly(ctx) {
		return prepared, domain.ErrDocRangeIndexUnavailable
	}
	if jobs := queries.view.RangeJobs; jobs != nil {
		return prepared, queries.interactiveViewWork(ctx, func(admitted context.Context) error {
			admitted, err := queries.scope.AuthorizeDocumentPreparation(admitted, repo)
			if err != nil {
				return err
			}
			var job domain.DocRangeIndexJob
			if requireBytes {
				preparation := queries.docByteDemand()
				if preparation == nil {
					return domain.ErrDocRangeIndexUnavailable
				}
				job, err = preparation.EnqueueDocByteRangeJob(admitted, repo, hash, time.Now().UTC())
			} else {
				job, err = jobs.EnqueueDocRangeJob(admitted, repo, hash, time.Now().UTC())
			}
			if err != nil {
				return err
			}
			return docRangePreparationError(job)
		})
	}
	admitted, release, err := queries.acquireViewBulk(ctx)
	if err != nil {
		return prepared, err
	}
	defer release()
	if queries.view.LegacyPreparation == nil {
		return prepared, domain.ErrDocRangeIndexUnavailable
	}
	admitted, err = queries.scope.AuthorizeDocumentPreparation(admitted, repo)
	if err != nil {
		return prepared, err
	}
	return prepared, queries.view.LegacyPreparation.EnsureDocRangeIndex(admitted, repo, hash)
}

func (queries *Queries) reauthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return queries.scope.AuthorizeDocumentRead(queries.readContext(ctx), repo)
}

func (queries *Queries) authorizeVisibleRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	ctx = queries.readContext(ctx)
	if inbound.DocumentReadAuthorizer(ctx) == nil && inbound.DocumentSnapshotReadAuthorizer(ctx) == nil {
		if _, system := inbound.RepositoryActor(ctx); !system {
			return ctx, domain.ErrUnauthorized
		}
	}
	return queries.scope.AuthorizeDocumentRead(ctx, repo)
}

func docRangePreparationError(job domain.DocRangeIndexJob) error {
	if job.Reason == "byte_backfill_required" {
		return domain.ErrDocByteRangeBackfillRequired
	}
	if job.State == "completed" {
		return nil
	}
	if job.State == "rejected" {
		if job.Reason == "source_unavailable" {
			return domain.ErrNotFound
		}
		return domain.ErrIntegrity
	}
	return domain.ErrDocRangeIndexPending
}

func preparedDocViewRead[Result any](ctx context.Context, queries *Queries, repo, hash, base domain.ContentHash, read func() (Result, error)) (Result, error) {
	prepare := func(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
		requirement := docRangeLegacyBytes
		if target != hash {
			requirement = docRangeLegacyMetadata
		}
		return queries.prepareDocViewSource(ctx, repo, target, requirement)
	}
	return preparedDocumentProjectionRead(ctx, queries, repo, hash, base, prepare, read)
}

func preparedDocVisibleRead[Result any](ctx context.Context, queries *Queries, repo, hash, base domain.ContentHash, read func() (Result, error)) (Result, error) {
	prepare := func(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
		requirement := docRangeRootOnly
		if target != hash {
			requirement = docRangeLegacyMetadata
		}
		_, err := queries.prepareDocVisibleSource(ctx, repo, target, requirement)
		return true, err
	}
	return preparedDocumentProjectionRead(ctx, queries, repo, hash, base, prepare, read)
}

func preparedDocumentProjectionRead[Result any](ctx context.Context, queries *Queries, repo, hash, base domain.ContentHash, prepareSource func(context.Context, domain.ContentHash, domain.ContentHash) (bool, error), read func() (Result, error)) (Result, error) {
	ctx = queries.readContext(ctx)
	var empty Result
	var preparedPair bool
	boundaries := queries.docBoundaryRead() != nil
	prepare := func() error {
		sourcePrepared, err := prepareSource(ctx, repo, hash)
		if err != nil && !errors.Is(err, domain.ErrDocRangeIndexPending) {
			return err
		}
		if base != "" {
			basePrepared, baseErr := prepareSource(ctx, repo, base)
			if baseErr != nil && !errors.Is(baseErr, domain.ErrDocRangeIndexPending) {
				return baseErr
			}
			preparedPair = boundaries && sourcePrepared && basePrepared
			if err != nil || baseErr != nil {
				if preparedPair {
					if boundaryErr := queries.prepareDocViewBoundary(ctx, repo, hash, base); boundaryErr != nil {
						return boundaryErr
					}
				}
				return domain.ErrDocRangeIndexPending
			}
		}
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := prepare(); err != nil {
			return empty, err
		}
		result, err := read()
		boundaryMissing := errors.Is(err, errDocViewBoundaryNeedsPreparation)
		generationMissing := errors.Is(err, errDocViewGenerationNeedsPreparation)
		if !boundaryMissing && !generationMissing {
			return result, err
		}
		if outbound.DocReadOnly(ctx) {
			return empty, domain.ErrDocRangeIndexUnavailable
		}
		if err = prepare(); err == nil && (boundaryMissing || preparedPair) {
			err = queries.prepareDocViewBoundary(ctx, repo, hash, base)
		}
		if err != nil {
			return empty, err
		}
	}
	return empty, domain.ErrDocRangeIndexPending
}

func (queries *Queries) prepareDocViewBoundary(ctx context.Context, repo, hash, base domain.ContentHash) error {
	ctx = queries.readContext(ctx)
	storage := queries.docBoundaryDemand()
	if storage == nil || outbound.DocReadOnly(ctx) {
		return domain.ErrDocRangeIndexUnavailable
	}
	return queries.interactiveViewWork(ctx, func(admitted context.Context) error {
		admitted, err := queries.scope.AuthorizeDocumentPreparation(admitted, repo)
		if err != nil {
			return err
		}
		job, err := storage.EnqueueDocRangeBoundary(admitted, repo, hash, base, time.Now().UTC())
		if err != nil {
			return err
		}
		return docRangePreparationError(job)
	})
}
