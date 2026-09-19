//go:build postgres

package store

import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"

// A cloud build cannot drop a required port during adapter refactoring.
var _ outbound.ProductionStore = (*PostgresStore)(nil)
