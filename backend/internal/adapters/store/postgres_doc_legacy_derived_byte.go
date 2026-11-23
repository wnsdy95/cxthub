//go:build postgres

package store

import (
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) ConfigureLegacyDerivedByteRangePublication(enabled bool) {
}

var _ outbound.LegacyDerivedByteRangePublicationStore = (*PostgresStore)(nil)
