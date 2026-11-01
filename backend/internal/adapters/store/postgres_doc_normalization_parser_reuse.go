//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) stageLegacyNormalizationParserObject(ctx context.Context, job domain.DocRangeIndexJob, expected legacyByteCheckpoint, hash domain.ContentHash, body []byte) error {
	return storage.stageLegacyNormalizationCheckpointObjects(ctx, job, expected, []legacyByteCheckpointObject{{hash: hash, data: body}})
}
