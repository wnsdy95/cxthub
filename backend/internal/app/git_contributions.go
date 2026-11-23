package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type gitContributionCandidate struct {
	publication          domain.HistoryEvent
	sourceObservations   []string
	carryObservations    []string
	sourceHead           string
	possibleIntroduction string
	memorySource         domain.ContentHash
}
