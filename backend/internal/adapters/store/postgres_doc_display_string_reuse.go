//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docStringDisplayCompletedPeerSQL = `SELECT substring(checkpoint_hash FROM 1 FOR 72),substring(checkpoint FROM 1 FOR $2)
 FROM doc_display_jobs_v1 WHERE source_hash=$1
 AND profile='tool-result-string-v1' AND worker_protocol=2 AND state='completed'
 AND checkpoint_hash IS NOT NULL AND checkpoint IS NOT NULL
 ORDER BY repo_id,doc_hash,event_index LIMIT 1`

func readCompletedDocDisplayString(ctx context.Context, transaction pgx.Tx, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, bool, error) {
	published, err := readDocDisplayPublished(ctx, transaction, source.EventHash, source.Profile)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.DocDisplayPreparation{}, false, nil
	}
	if err != nil {
		return domain.DocDisplayPreparation{}, false, err
	}
	owner, err := source.Hash()
	if err != nil {
		return domain.DocDisplayPreparation{}, false, err
	}
	generation, err := domain.DecodeDocDisplayStringGeneration(published.generation, published.raw)
	if err != nil || published.owner != owner || generation.Source != source {
		return domain.DocDisplayPreparation{}, false, domain.ErrIntegrity
	}
	var hash domain.ContentHash
	var raw []byte
	err = transaction.QueryRow(ctx, docStringDisplayCompletedPeerSQL, owner, domain.MaxDocDisplayStateBytes+1).Scan(&hash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocDisplayPreparation{}, false, nil
	}
	if err != nil {
		return domain.DocDisplayPreparation{}, false, err
	}
	state, err := domain.DecodeDocDisplayStringState(source, hash, raw)
	if err != nil || !state.Done {
		return domain.DocDisplayPreparation{}, false, domain.ErrIntegrity
	}
	finished, _, err := domain.FinishDocDisplayString(ctx, source, state)
	if err != nil {
		return domain.DocDisplayPreparation{}, false, err
	}
	encoded, err := finished.Encode()
	if err != nil || domain.HashContent(encoded) != published.generation || !bytes.Equal(encoded, published.raw) {
		return domain.DocDisplayPreparation{}, false, domain.ErrIntegrity
	}
	return domain.DocDisplayPreparation{Source: &source, State: state, Checkpoint: hash}, true, ctx.Err()
}

func (storage *PostgresStore) reuseDocDisplayString(ctx context.Context, transaction pgx.Tx, job domain.DocDisplayJob, prior domain.DocDisplayPreparation) (domain.DocDisplayPreparation, error) {
	if prior.Source == nil || prior.State.Done {
		return prior, nil
	}
	shared, reusable, err := readCompletedDocDisplayString(ctx, transaction, *prior.Source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if !reusable {
		return prior, nil
	}
	var checkpoint, discovery, owner any
	if prior.Checkpoint != "" {
		checkpoint, owner = prior.Checkpoint, prior.State.Source
	}
	if prior.DiscoveryCheckpoint != "" {
		discovery = prior.DiscoveryCheckpoint
	}
	raw, err := shared.State.Encode(*shared.Source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	tag, err := transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET source_hash=$4,checkpoint_hash=$5,checkpoint=$6,discovery_hash=NULL,discovery=NULL
 WHERE `+docDisplayKeySQL+` AND checkpoint_hash IS NOT DISTINCT FROM $8 AND discovery_hash IS NOT DISTINCT FROM $9 AND source_hash IS NOT DISTINCT FROM $10
 AND state='running' AND version=$7 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Target.Event.Index, shared.State.Source, shared.Checkpoint, raw, job.Version, checkpoint, discovery, owner)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.DocDisplayPreparation{}, domain.ErrConflict
	}
	return shared, nil
}
