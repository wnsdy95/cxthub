package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GraphProjectionStore interface {
	ConfigureGraphProjection(context.Context, domain.ContentHash, bool) error
	ClaimGraphProjection(context.Context, time.Duration) (domain.GraphProjectionLease, error)
	RenewGraphProjection(context.Context, domain.GraphProjectionLease, time.Duration) error
	SaveGraphProjectionPage(context.Context, domain.GraphProjectionLease, domain.RepositoryViewSourcePage) error
	LoadGraphProjectionInputs(context.Context, domain.GraphProjectionLease) ([]domain.RepositoryViewSourceEntry, error)
	PublishGraphProjection(context.Context, domain.GraphProjectionLease, []domain.GraphProjectionPart) error
	FailGraphProjection(context.Context, domain.GraphProjectionLease, string, bool) error
	ReadGraphProjection(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
}

type GraphProjectionStatusStore interface {
	ReadGraphProjectionStatus(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
}

type GraphProjectionReferenceStore interface {
	SaveGraphProjectionPageWithReference(context.Context, domain.GraphProjectionLease, domain.RepositoryViewSourcePage, domain.GraphProjectionReference) error
}

type GraphProjectionRepairStore interface {
	RequestGraphProjectionRepair(context.Context, domain.GraphProjectionCorruption) error
}

type GraphServingStore interface {
	ReadQualifiedGraphProjection(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
	ReadQualifiedGraphProjectionStatus(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error)
	RequestGraphServingRepair(context.Context, domain.GraphProjectionCorruption) error
}

type GraphQualificationStore interface {
	CheckGraphQualificationInputs(context.Context, domain.ContentHash) error
	ConfigureGraphQualification(context.Context, domain.ContentHash, bool) error
	ClaimGraphQualification(context.Context, domain.ContentHash, time.Duration) (domain.GraphQualificationLease, error)
	FinishGraphQualification(context.Context, domain.GraphQualificationLease, domain.GraphQualificationResult) error
	FailGraphQualification(context.Context, domain.GraphQualificationLease, string) error
	ReadGraphQualification(context.Context, domain.GraphQualificationKey) (domain.GraphQualificationResult, error)
}

type GraphSourceReaderFactory interface {
	BuildGraphSourceReader(context.Context, domain.RepositoryViewSourceCheckpoint, domain.Repo, []domain.RepositoryViewSourceEntry) (GraphEvidenceReader, error)
}
