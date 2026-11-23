package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docFindPreparedWindows = 8

type docDisplayPreparationRequired struct {
	target domain.DocDisplayTarget
}

func (failure *docDisplayPreparationRequired) Error() string {
	return "display preparation is required"
}
