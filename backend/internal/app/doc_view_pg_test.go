//go:build postgres

package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

type docViewPGStore struct {
	*store.PostgresStore
	opens, pages, fallbackPages int
}
