package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitHeadReader interface {
	ReadGitHead(context.Context, string, string) (GitHead, error)
}

type GitHeadSourceScanStore interface {
	FinishGitHeadScanWithSources(context.Context, domain.GitHeadScan, []domain.GitRefObservation, []domain.GitRefObservation, bool, time.Time) error
}
