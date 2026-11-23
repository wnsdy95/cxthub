package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docPreparedFindEvent struct {
	hash  domain.ContentHash
	pages int
}
