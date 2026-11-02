//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docEventDisplayCompletedPeerSQL = `SELECT substring(preparation_hash FROM 1 FOR 72),substring(preparation FROM 1 FOR $2),catalog_epoch::text
 FROM doc_display_jobs_v1 WHERE source_hash=$1
 AND profile='public-event-display-v1' AND worker_protocol=5 AND state='completed'
 AND preparation_hash IS NOT NULL AND preparation IS NOT NULL
 ORDER BY repo_id,doc_hash,event_index LIMIT 1`

type docDisplayPublished struct {
	owner, generation domain.ContentHash
	raw               []byte
}

func readDocDisplayPublished(ctx context.Context, transaction pgx.Tx, event domain.ContentHash, profile string) (docDisplayPublished, error) {
	var result docDisplayPublished
	err := transaction.QueryRow(ctx, `SELECT substring(generation_hash FROM 1 FOR 72),substring(source_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $3)
 FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, event, profile, domain.MaxDocDisplaySourceBytes+1).Scan(&result.generation, &result.owner, &result.raw)
	return result, mapNoRows(err)
}

func readCompletedDocEventDisplay(ctx context.Context, transaction pgx.Tx, source domain.DocEventDisplaySource, owner domain.ContentHash) (domain.DocEventDisplayPreparation, string, bool, error) {
	published, err := readDocDisplayPublished(ctx, transaction, source.EventHash, source.Profile)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.DocEventDisplayPreparation{}, "", false, nil
	}
	if err != nil {
		return domain.DocEventDisplayPreparation{}, "", false, err
	}
	generation, err := domain.DecodeDocDisplayProjectionGeneration(published.generation, published.raw)
	if err != nil || published.owner != owner || generation.Source.EventHash != source.EventHash || generation.Source.EventBytes != source.EventBytes || source.EmptyReasoning && generation.Source.TextBytes != 0 {
		return domain.DocEventDisplayPreparation{}, "", false, domain.ErrIntegrity
	}
	var checkpoint domain.ContentHash
	var raw []byte
	var epoch *string
	err = transaction.QueryRow(ctx, docEventDisplayCompletedPeerSQL, owner, domain.MaxDocEventDisplayPreparationBytes+1).Scan(&checkpoint, &raw, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocEventDisplayPreparation{}, "", false, nil
	}
	if err != nil {
		return domain.DocEventDisplayPreparation{}, "", false, err
	}
	state, err := domain.DecodeDocEventDisplayPreparation(checkpoint, raw)
	if err != nil || state.Phase != "completed" || state.Source != source || state.Generation != published.generation || state.Plan != generation.Source.Plan || epoch == nil || domain.ValidateDocDisplayCatalogEpoch(*epoch) != nil {
		return domain.DocEventDisplayPreparation{}, "", false, domain.ErrIntegrity
	}
	return state, *epoch, true, ctx.Err()
}
