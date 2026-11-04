package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocRangeWorkStore interface {
	PrepareDocRangeWork(context.Context, domain.DocRangeIndexJob) (PreparedDocRangeWork, error)
	BuildDocRangeCompatibilityJob(context.Context, domain.DocRangeIndexJob) error
}

type DocRangeWorkProgress uint8

const (
	DocRangeWorkPending DocRangeWorkProgress = iota + 1
	DocRangeWorkPublishable
	DocRangeWorkCompatibilityRequired
)

type PreparedDocRangeWork interface {
	Step(context.Context, domain.DocRangeIndexJob) (DocRangeWorkProgress, error)
	Publish(context.Context, domain.DocRangeIndexJob) error
	RetainedResources() (RootPublicationResources, error)
	Close() error
}
