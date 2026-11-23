package app

import (
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextContributionProjection struct {
	valid             bool
	publications      map[string]domain.HistoryEvent
	sourceBranch      string
	destinationBranch string
	prNumber          int
	mergedAt          time.Time
}
