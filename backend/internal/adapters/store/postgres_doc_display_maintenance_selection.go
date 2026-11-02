//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docDisplayMaintenanceCandidates = 8

var errDocDisplayMaintenanceCandidateBusy = errors.New("document display maintenance candidate is busy")

type docDisplayMaintenanceCandidate struct {
	source domain.ContentHash
	ready  bool
}

func (storage *PostgresStore) selectDocDisplayMaintenance(ctx context.Context) (docDisplayMaintenanceCandidate, error) {
	candidate := docDisplayMaintenanceCandidate{}
	ctx, cancel := context.WithTimeout(ctx, docRangeEnqueueTimeout)
	defer cancel()
	transaction, err := storage.beginDocRangeJob(ctx)
	if err != nil {
		return candidate, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout='500ms'; SET LOCAL statement_timeout='500ms'`); err != nil {
		return candidate, err
	}
	after, candidate, found, err := readDocDisplayMaintenanceRange(ctx, transaction, `source_hash>locked_cursor.after_source`)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_maintenance_cursor_v1(singleton) VALUES(true) ON CONFLICT DO NOTHING`); err != nil {
			return candidate, err
		}
		after, candidate, found, err = readDocDisplayMaintenanceRange(ctx, transaction, `source_hash>locked_cursor.after_source`)
	}
	if err != nil {
		return candidate, docDisplayMaintenanceAcquisitionError(err)
	}
	if !found {
		var current string
		current, candidate, found, err = readDocDisplayMaintenanceRange(ctx, transaction, `source_hash<=locked_cursor.after_source`)
		if err != nil {
			return docDisplayMaintenanceCandidate{}, docDisplayMaintenanceAcquisitionError(err)
		}
		if current != after {
			return docDisplayMaintenanceCandidate{}, domain.ErrConflict
		}
	}
	if !found {
		if err := transaction.Commit(ctx); err != nil {
			return candidate, err
		}
		return candidate, domain.ErrNotFound
	}
	if string(candidate.source) != after {
		if _, err := transaction.Exec(ctx, `UPDATE doc_display_maintenance_cursor_v1 SET after_source=$1 WHERE singleton`, candidate.source); err != nil {
			return candidate, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return candidate, err
	}
	return candidate, nil
}

func readDocDisplayMaintenanceRange(ctx context.Context, transaction pgx.Tx, predicate string) (string, docDisplayMaintenanceCandidate, bool, error) {
	var after string
	var source *domain.ContentHash
	var ready *bool
	err := transaction.QueryRow(ctx, `SELECT locked_cursor.after_source,work.source_hash,work.ready
 FROM (SELECT after_source FROM doc_display_maintenance_cursor_v1 WHERE singleton FOR UPDATE NOWAIT) locked_cursor
 LEFT JOIN LATERAL (SELECT source_hash,due_at<=clock_timestamp() AS ready FROM doc_display_maintenance_v1
 WHERE phase<>'idle' AND `+predicate+` ORDER BY source_hash LIMIT $1) work ON true`, 1).Scan(&after, &source, &ready)
	if err != nil {
		return "", docDisplayMaintenanceCandidate{}, false, err
	}
	if source == nil {
		if ready != nil {
			return "", docDisplayMaintenanceCandidate{}, false, domain.ErrIntegrity
		}
		return after, docDisplayMaintenanceCandidate{}, false, nil
	}
	if ready == nil {
		return "", docDisplayMaintenanceCandidate{}, false, domain.ErrIntegrity
	}
	return after, docDisplayMaintenanceCandidate{source: *source, ready: *ready}, true, nil
}

func lockDocDisplayMaintenanceSource(ctx context.Context, transaction pgx.Tx, hash domain.ContentHash) (domain.DocDisplayCatalogSource, int64, error) {
	source := domain.DocDisplayCatalogSource{Hash: hash}
	if err := transaction.QueryRow(ctx, `SELECT event_hash,profile FROM doc_display_sources_v1 WHERE source_hash=$1`, hash).Scan(&source.EventHash, &source.Profile); err != nil {
		return source, 0, mapNoRows(err)
	}
	if _, err := transaction.Exec(ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, source.EventHash, source.Profile); err != nil {
		return source, 0, err
	}
	var unused bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, docDisplayCatalogUseKey(source.EventHash, source.Profile)).Scan(&unused); err != nil {
		return source, 0, err
	}
	if !unused {
		return source, 0, domain.ErrResourceOverloaded
	}
	var revision int64
	current := domain.DocDisplayCatalogSource{Hash: hash}
	if err := transaction.QueryRow(ctx, `SELECT event_hash,profile,bytes,reference_revision FROM doc_display_sources_v1
 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`, hash).Scan(&current.EventHash, &current.Profile, &current.Bytes, &revision); err != nil {
		return source, 0, mapNoRows(err)
	}
	if current.EventHash != source.EventHash || current.Profile != source.Profile {
		return source, 0, domain.ErrConflict
	}
	return current, revision, nil
}

func docDisplayMaintenanceAcquisitionError(err error) error {
	var postgresError *pgconn.PgError
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrResourceOverloaded) ||
		(errors.As(err, &postgresError) && postgresError.Code == "55P03") {
		return fmt.Errorf("%w: %w", errDocDisplayMaintenanceCandidateBusy, err)
	}
	return err
}
