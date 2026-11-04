package documentquery

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type DocRangeReader interface {
	DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error)
	DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error)
}

type LegacyRangePreparation interface {
	EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error
}

type DocRangeEnqueuer interface {
	EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type DocVisibleReadiness interface {
	HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
}

type DocByteRangeReadiness interface {
	LegacyByteRangePublicationEnabled() bool
	HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
}

type DocByteRangeDemand interface {
	EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type DocRangeBoundaryReader interface {
	InReadOnlyTransaction(context.Context) bool
	DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error)
}

type DocRangeBoundaryDemand interface {
	EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}

type ViewResources interface {
	AcquireInteractive(context.Context) (context.Context, func(), error)
	AcquireBulk(context.Context) (context.Context, func(), error)
}

type ViewDependencies struct {
	Range               DocRangeReader
	LegacyPreparation   LegacyRangePreparation
	RangeSnapshots      outbound.DocRangeSnapshotStore
	RangeGenerations    outbound.DocRangeGenerationStore
	RangeReadiness      outbound.DocRangeReadinessStore
	RangeJobs           DocRangeEnqueuer
	VisibleGenerations  outbound.DocVisibleGenerationStore
	VisibleReadiness    DocVisibleReadiness
	VisiblePreparation  outbound.DocVisiblePreparationStore
	BoundaryRead        DocRangeBoundaryReader
	BoundaryPreparation DocRangeBoundaryDemand
	Boundaries          outbound.DocRangeBoundaryStore
	Chunks              outbound.ConversationChunkReader
	DerivedChunks       outbound.LegacyDerivedByteRangeReader
	Bytes               outbound.DocByteRangePreparationStore
	ByteReadiness       DocByteRangeReadiness
	BytePreparation     DocByteRangeDemand
	Resources           ViewResources
}

func (dependencies ViewDependencies) validate(profile Profile, readOnly bool) error {
	if absent(dependencies.Resources) {
		return fmt.Errorf("%w: document view resources are required", domain.ErrValidation)
	}
	for _, optional := range []any{dependencies.Range, dependencies.LegacyPreparation, dependencies.RangeSnapshots,
		dependencies.RangeGenerations, dependencies.RangeReadiness, dependencies.RangeJobs, dependencies.VisibleGenerations,
		dependencies.VisiblePreparation, dependencies.Boundaries, dependencies.Chunks, dependencies.DerivedChunks, dependencies.Bytes,
		dependencies.VisibleReadiness, dependencies.BoundaryRead, dependencies.BoundaryPreparation, dependencies.ByteReadiness, dependencies.BytePreparation} {
		if optional != nil && absent(optional) {
			return fmt.Errorf("%w: typed-nil document view dependency", domain.ErrValidation)
		}
	}
	if readOnly {
		for _, demand := range []any{dependencies.LegacyPreparation, dependencies.RangeJobs, dependencies.VisiblePreparation,
			dependencies.Boundaries, dependencies.Bytes, dependencies.BoundaryPreparation, dependencies.BytePreparation} {
			if demand != nil {
				return fmt.Errorf("%w: read-only document queries cannot retain preparation dependencies", domain.ErrValidation)
			}
		}
	}
	if profile == VerifiedRoots {
		if dependencies.LegacyPreparation != nil {
			return fmt.Errorf("%w: verified queries cannot run legacy index preparation", domain.ErrValidation)
		}
		requiredPorts := []any{dependencies.Range, dependencies.RangeGenerations, dependencies.RangeReadiness, dependencies.VisibleGenerations, dependencies.Chunks}
		if readOnly {
			requiredPorts = append(requiredPorts, dependencies.VisibleReadiness, dependencies.BoundaryRead)
		} else {
			requiredPorts = append(requiredPorts, dependencies.RangeJobs, dependencies.VisiblePreparation, dependencies.Boundaries)
		}
		for _, required := range requiredPorts {
			if absent(required) {
				return domain.ErrUnsupportedDocumentIdentity
			}
		}
	}
	return nil
}

func (queries *Queries) visibleReadiness() DocVisibleReadiness {
	if queries.view.VisibleReadiness != nil {
		return queries.view.VisibleReadiness
	}
	return queries.view.VisiblePreparation
}

func (queries *Queries) docBoundaryRead() DocRangeBoundaryReader {
	if queries.view.BoundaryRead != nil {
		return queries.view.BoundaryRead
	}
	return queries.view.Boundaries
}

func (queries *Queries) docBoundaryDemand() DocRangeBoundaryDemand {
	if queries.view.BoundaryPreparation != nil {
		return queries.view.BoundaryPreparation
	}
	return queries.view.Boundaries
}

func (queries *Queries) docByteDemand() DocByteRangeDemand {
	if queries.view.BytePreparation != nil {
		return queries.view.BytePreparation
	}
	return queries.view.Bytes
}

func (queries *Queries) usesLegacyRangeReadiness() bool {
	return queries.view.RangeJobs != nil || queries.readOnly && queries.view.RangeReadiness != nil
}

func (queries *Queries) acquireViewInteractive(ctx context.Context) (context.Context, func(), error) {
	ctx = queries.readContext(ctx)
	if queries.view.Resources == nil {
		return ctx, nil, domain.ErrDocRangeIndexUnavailable
	}
	return queries.view.Resources.AcquireInteractive(ctx)
}

func (queries *Queries) acquireViewBulk(ctx context.Context) (context.Context, func(), error) {
	ctx = queries.readContext(ctx)
	if queries.view.Resources == nil {
		return ctx, nil, domain.ErrDocRangeIndexUnavailable
	}
	return queries.view.Resources.AcquireBulk(ctx)
}

func (queries *Queries) interactiveViewWork(ctx context.Context, work func(context.Context) error) error {
	bound, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work(bound)
}

func validateHashes(hashes ...domain.ContentHash) error {
	for _, hash := range hashes {
		if err := domain.ValidateContentHash(hash); err != nil {
			return err
		}
	}
	return nil
}
