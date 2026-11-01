//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type projectionSpec struct {
	scope                      string
	rules                      int
	work, inputs, parts, ready string
	sourceSQL                  string
	kindValid                  func(string) bool
	decodeParts                func(context.Context, domain.ContentHash, []domain.PendingProjectionPart) error
	partKeys                   func(domain.PendingProjectionPart) ([]string, []string, error)
	repairQuarantined          func(context.Context, pgx.Tx, domain.ContentHash) (bool, error)
	claimHeldSQL               string
}

type projectionStore struct {
	*PostgresStore
	spec projectionSpec
}

type projectionReadOptions struct {
	includeParts       bool
	allowPrior         bool
	retainFailedOutput bool
}

type projectionRead struct {
	domain.PendingProjectionRead
	complete bool
	rules    int
}

func (s projectionStore) latestInputs() string {
	return `SELECT source_seq,kind,entity_key,deleted,image FROM ` + s.spec.inputs + `
 WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 ORDER BY kind,entity_key`
}

func (s projectionStore) latestParts() string {
	return `SELECT source_seq,kind,entity_key,deleted,image FROM ` + s.spec.parts + `
 WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 ORDER BY kind,entity_key`
}

const projectionWorkColumns = `work.repo_id,work.dataset::text,work.source_epoch::text,
	work.fence,work.phase,work.applied_seq,work.input_seq,work.cursor,work.lease_until,work.attempts,
	work.enabled,work.pin_seq,work.retention_until>clock_timestamp(),COALESCE(work.lease_until>clock_timestamp(),false)`

type projectionWork struct {
	domain.PendingProjectionLease
	enabled  bool
	pin      int64
	retained bool
	leased   bool
}

type projectionSource struct {
	epoch    string
	head     int64
	floor    int64
	baseline int64
}

func (s projectionStore) checkpoint(repo domain.ContentHash, epoch string, sequence int64) domain.RepositoryViewSourceCheckpoint {
	return domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
		Scope: s.spec.scope, RepoID: repo, Epoch: epoch, Sequence: sequence}
}

func scanProjectionWork(row pgx.Row) (projectionWork, error) {
	var work projectionWork
	var until *time.Time
	err := row.Scan(&work.RepoID, &work.Dataset, &work.Epoch, &work.Fence, &work.Phase,
		&work.AppliedSequence, &work.InputSequence, &work.Cursor, &until, &work.Attempts,
		&work.enabled, &work.pin, &work.retained, &work.leased)
	if until != nil {
		work.LeaseUntil = *until
	}
	return work, mapNoRows(err)
}

func (s projectionStore) readSource(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, lock bool) (projectionSource, error) {
	var source projectionSource
	var sourceEpoch string
	query := s.spec.sourceSQL
	if lock {
		query += ` FOR SHARE OF catalog`
	}
	if err := database.QueryRow(ctx, query, string(repo)).Scan(&source.epoch, &source.head, &source.floor, &sourceEpoch, &source.baseline); err != nil {
		return source, mapNoRows(err)
	}
	if sourceEpoch != source.epoch || source.floor < 0 || source.floor > source.head || source.baseline < 0 || source.baseline > source.head || s.checkpoint(repo, source.epoch, source.head).Validate() != nil {
		return source, domain.ErrIntegrity
	}
	return source, nil
}

func (s projectionStore) begin(ctx context.Context) (pgx.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.InTransaction(ctx) || outbound.DocReadOnly(ctx) {
		return nil, domain.ErrConflict
	}
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

func (s projectionStore) leaseValid(lease domain.PendingProjectionLease) bool {
	return s.checkpoint(lease.RepoID, lease.Epoch, 0).Validate() == nil &&
		s.checkpoint(lease.RepoID, lease.Dataset, 0).Validate() == nil && lease.Fence > 0 &&
		lease.AppliedSequence >= -1 && lease.InputSequence >= -1 && len(lease.Cursor) <= 16<<10 &&
		((lease.Phase == "capture" && lease.InputSequence == -1) ||
			(lease.Phase == "project" && lease.InputSequence >= 0 && lease.InputSequence > lease.AppliedSequence && lease.Cursor == ""))
}

func projectionLeaseMatches(work projectionWork, lease domain.PendingProjectionLease) bool {
	return work.enabled && work.leased && work.retained && work.RepoID == lease.RepoID &&
		work.Dataset == lease.Dataset && work.Epoch == lease.Epoch && work.Fence == lease.Fence &&
		work.Phase == lease.Phase && work.AppliedSequence == lease.AppliedSequence &&
		work.InputSequence == lease.InputSequence && work.Cursor == lease.Cursor
}

func (s projectionStore) lockLease(ctx context.Context, transaction pgx.Tx, lease domain.PendingProjectionLease) (projectionWork, projectionSource, error) {
	work, err := scanProjectionWork(transaction.QueryRow(ctx, `SELECT `+projectionWorkColumns+`
		FROM `+s.spec.work+` work WHERE repo_id=$1 FOR UPDATE`, string(lease.RepoID)))
	if errors.Is(err, domain.ErrNotFound) {
		err = domain.ErrRefConflict
	}
	if err != nil {
		return work, projectionSource{}, err
	}
	if !projectionLeaseMatches(work, lease) {
		return work, projectionSource{}, domain.ErrRefConflict
	}
	source, err := s.readSource(ctx, transaction, lease.RepoID, true)
	if err != nil {
		return work, source, err
	}
	if source.epoch != lease.Epoch || source.floor > work.pin || source.head < max(lease.InputSequence, lease.AppliedSequence) ||
		(lease.AppliedSequence >= 0 && lease.AppliedSequence < source.baseline) ||
		(lease.Phase == "capture" && source.floor != work.pin) {
		return work, source, domain.ErrCatalogResetRequired
	}
	return work, source, nil
}

func (s projectionStore) pruneDatasets(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash) error {
	if _, err := transaction.Exec(ctx, `DELETE FROM `+s.spec.inputs+` obsolete USING `+s.spec.work+` work
		WHERE obsolete.repo_id=$1 AND work.repo_id=obsolete.repo_id AND obsolete.dataset<>work.dataset`, string(repo)); err != nil {
		return err
	}
	_, err := transaction.Exec(ctx, `DELETE FROM `+s.spec.parts+` obsolete USING `+s.spec.work+` work
		WHERE obsolete.repo_id=$1 AND work.repo_id=obsolete.repo_id AND obsolete.dataset<>work.dataset
		AND NOT EXISTS(SELECT 1 FROM `+s.spec.ready+` ready WHERE ready.repo_id=obsolete.repo_id AND ready.dataset=obsolete.dataset)`, string(repo))
	return err
}

func (s projectionStore) configure(ctx context.Context, repo domain.ContentHash, enabled bool) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `INSERT INTO `+s.spec.work+`(repo_id) VALUES($1) ON CONFLICT DO NOTHING`, string(repo)); err != nil {
		return mapPGConstraint(err)
	}
	work, err := scanProjectionWork(transaction.QueryRow(ctx, `SELECT `+projectionWorkColumns+`
		FROM `+s.spec.work+` work WHERE repo_id=$1 FOR UPDATE`, string(repo)))
	if err != nil {
		return err
	}
	source, err := s.readSource(ctx, transaction, repo, true)
	if err != nil {
		return err
	}
	if enabled && s.spec.repairQuarantined != nil {
		quarantined, err := s.spec.repairQuarantined(ctx, transaction, repo)
		if err != nil {
			return err
		}
		if quarantined {
			if _, err := transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET enabled=true WHERE repo_id=$1 AND NOT enabled`, string(repo)); err != nil {
				return err
			}
			return transaction.Commit(ctx)
		}
	}
	if enabled && (!work.enabled || work.Phase == "needs_attention") {
		_, err = transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET enabled=true,source_epoch=$2,dataset=gen_random_uuid(),
			applied_seq=-1,input_seq=-1,cursor='',phase='capture',lease_until=NULL,fence=fence+1,attempts=0,
			next_attempt=clock_timestamp(),reason='',pin_seq=$3,retention_until=clock_timestamp()+interval '24 hours' WHERE repo_id=$1`, string(repo), source.epoch, source.floor)
	} else if !enabled && work.enabled {
		_, err = transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET enabled=false,lease_until=NULL,fence=fence+1 WHERE repo_id=$1`, string(repo))
	}
	if err != nil {
		return err
	}
	if enabled && (!work.enabled || work.Phase == "needs_attention") {
		if err := s.pruneDatasets(ctx, transaction, repo); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func (s projectionStore) claim(ctx context.Context, duration time.Duration) (lease domain.PendingProjectionLease, failure error) {
	if duration < time.Second || duration > 5*time.Minute {
		return domain.PendingProjectionLease{}, domain.ErrValidation
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return domain.PendingProjectionLease{}, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		return domain.PendingProjectionLease{}, err
	}
	eligible := ""
	if s.spec.claimHeldSQL != "" {
		eligible = ` AND NOT (` + s.spec.claimHeldSQL + `)`
	}
	work, err := scanProjectionWork(transaction.QueryRow(ctx, `SELECT `+projectionWorkColumns+`
		FROM `+s.spec.work+` work JOIN repository_catalog_state catalog ON catalog.repo_id=work.repo_id
		WHERE work.enabled AND work.phase<>'needs_attention' AND work.next_attempt<=clock_timestamp()
		AND (work.lease_until IS NULL OR work.lease_until<=clock_timestamp())
		AND (work.phase IN ('capture','project') OR work.source_epoch<>catalog.epoch
			OR work.retention_until<=clock_timestamp() OR work.pin_seq<catalog.floor_seq
			OR (work.phase='idle' AND (work.applied_seq<catalog.head_seq OR NOT EXISTS(
				SELECT 1 FROM `+s.spec.ready+` ready WHERE ready.repo_id=work.repo_id AND ready.dataset=work.dataset
				AND ready.source_epoch=work.source_epoch AND ready.source_seq=work.applied_seq AND ready.rules=$1))))`+eligible+`
		ORDER BY work.next_attempt,work.repo_id FOR UPDATE OF work SKIP LOCKED LIMIT 1`, s.spec.rules))
	if err != nil {
		return domain.PendingProjectionLease{}, err
	}
	recordFailure := true
	defer func(candidate projectionWork) {
		if failure == nil || ctx.Err() != nil || !recordFailure {
			return
		}
		rollbackPG(transaction)
		failure = errors.Join(failure, s.failClaim(ctx, candidate, failure))
	}(work)
	source, err := s.readSource(ctx, transaction, work.RepoID, true)
	if err != nil {
		return domain.PendingProjectionLease{}, err
	}
	if err := transaction.QueryRow(ctx, `SELECT retention_until>clock_timestamp() FROM `+s.spec.work+` WHERE repo_id=$1`, string(work.RepoID)).Scan(&work.retained); err != nil {
		return domain.PendingProjectionLease{}, err
	}
	reset := work.Epoch != source.epoch || !work.retained || work.pin < source.floor ||
		work.AppliedSequence > source.head || work.InputSequence > source.head ||
		(work.AppliedSequence >= 0 && work.AppliedSequence < source.baseline)
	if !reset && work.AppliedSequence >= 0 {
		var ready bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.spec.ready+`
			WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$5)`,
			string(work.RepoID), work.Dataset, work.Epoch, work.AppliedSequence, s.spec.rules).Scan(&ready); err != nil {
			return domain.PendingProjectionLease{}, err
		}
		reset = !ready
	}
	if work.Phase == "capture" {
		reset = reset || work.pin != source.floor
		if !reset && work.Cursor == "" {
			if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.spec.inputs+`
				WHERE repo_id=$1 AND dataset=$2 AND source_seq>$3)`, string(work.RepoID), work.Dataset, work.AppliedSequence).Scan(&reset); err != nil {
				return domain.PendingProjectionLease{}, err
			}
		}
	}
	if work.Cursor != "" {
		cursor, err := decodeRepositoryViewSourceCursor(work.Cursor)
		reset = reset || err != nil || cursor.Checkpoint.Scope != s.spec.scope || cursor.Checkpoint.RepoID != work.RepoID || cursor.Checkpoint.Epoch != source.epoch ||
			cursor.Checkpoint.Sequence > source.head || cursor.Checkpoint.Sequence < max(source.floor, source.baseline) ||
			cursor.Floor != source.floor || cursor.Baseline != source.baseline
	}
	if !reset && work.Phase == "idle" && s.spec.claimHeldSQL != "" {
		var held bool
		if err := transaction.QueryRow(ctx, `SELECT (`+s.spec.claimHeldSQL+`) FROM `+s.spec.work+` work
			JOIN repository_catalog_state catalog ON catalog.repo_id=work.repo_id WHERE work.repo_id=$2`, s.spec.rules, string(work.RepoID)).Scan(&held); err != nil {
			return domain.PendingProjectionLease{}, err
		}
		if held {
			recordFailure = false
			return domain.PendingProjectionLease{}, domain.ErrNotFound
		}
	}
	if reset {
		_, err = transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET source_epoch=$2,dataset=gen_random_uuid(),
			applied_seq=-1,input_seq=-1,cursor='',phase='capture',pin_seq=$3,attempts=0,reason='' WHERE repo_id=$1`, string(work.RepoID), source.epoch, source.floor)
	} else if work.Phase == "idle" {
		_, err = transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET phase='capture',input_seq=-1,cursor='',pin_seq=$2 WHERE repo_id=$1`, string(work.RepoID), source.floor)
	}
	if err != nil {
		return domain.PendingProjectionLease{}, err
	}
	if reset {
		if err := s.pruneDatasets(ctx, transaction, work.RepoID); err != nil {
			return domain.PendingProjectionLease{}, err
		}
	}
	work, err = scanProjectionWork(transaction.QueryRow(ctx, `UPDATE `+s.spec.work+` AS work
		SET fence=fence+1,attempts=LEAST(attempts,2147483646)+1,lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond',
		retention_until=clock_timestamp()+interval '24 hours',next_attempt=clock_timestamp()
		WHERE repo_id=$1 RETURNING `+projectionWorkColumns, string(work.RepoID), duration.Microseconds()))
	if err != nil {
		return domain.PendingProjectionLease{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.PendingProjectionLease{}, err
	}
	return work.PendingProjectionLease, nil
}

func (s projectionStore) failClaim(ctx context.Context, work projectionWork, failure error) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	phase, reason := work.Phase, "temporary_failure"
	switch {
	case errors.Is(failure, domain.ErrDocumentIdentityUpgradeRequired):
		reason = "identity_upgrade_required"
	case errors.Is(failure, domain.ErrCatalogResetRequired):
		reason = "source_reset"
	case errors.Is(failure, domain.ErrRefConflict), errors.Is(failure, domain.ErrConflict):
		reason = "lease_lost"
	case errors.Is(failure, domain.ErrRepositoryViewSourceLimit):
		phase, reason = "needs_attention", "resource_limit"
	case errors.Is(failure, domain.ErrIntegrity), errors.Is(failure, domain.ErrValidation), errors.Is(failure, domain.ErrNotFound):
		phase, reason = "needs_attention", "invalid_source"
	case errors.Is(failure, domain.ErrForbidden):
		phase, reason = "needs_attention", "access_denied"
	}
	delay := min(int64(300), int64(1)<<min(max(work.Attempts, 0), 9))
	result, err := s.pool.Exec(ctx, `UPDATE `+s.spec.work+` SET phase=$9,reason=$10,lease_until=NULL,
		fence=fence+1,attempts=LEAST(attempts,2147483646)+1,next_attempt=clock_timestamp()+$11::bigint*interval '1 second'
		WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3 AND fence=$4 AND phase=$5
		AND applied_seq=$6 AND input_seq=$7 AND cursor=$8 AND enabled
		AND (lease_until IS NULL OR lease_until<=clock_timestamp())`, string(work.RepoID), work.Dataset, work.Epoch,
		work.Fence, work.Phase, work.AppliedSequence, work.InputSequence, work.Cursor, phase, reason, delay)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return nil
}

func (s projectionStore) renew(ctx context.Context, lease domain.PendingProjectionLease, duration time.Duration) error {
	if !s.leaseValid(lease) || duration < time.Second || duration > 5*time.Minute {
		return domain.ErrValidation
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, _, err := s.lockLease(ctx, transaction, lease); err != nil {
		return err
	}
	result, err := transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond',
		retention_until=clock_timestamp()+interval '24 hours' WHERE repo_id=$1 AND lease_until>clock_timestamp()
		AND retention_until>clock_timestamp()`, string(lease.RepoID), duration.Microseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}

func (s projectionStore) validatePage(work projectionWork, source projectionSource, page domain.RepositoryViewSourcePage) error {
	checkpoint := s.checkpoint(work.RepoID, work.Epoch, page.Through)
	if page.Version != checkpoint.Version || page.Scope != checkpoint.Scope || page.RepoID != checkpoint.RepoID || page.Epoch != checkpoint.Epoch ||
		checkpoint.Validate() != nil || page.Through > source.head || page.Through < max(source.floor, source.baseline) ||
		page.Through <= work.AppliedSequence || len(page.Entries) > domain.MaxRepositoryViewSourceLimit {
		return domain.ErrValidation
	}
	cursor := repositoryViewSourceCursor{Checkpoint: checkpoint, Floor: work.pin, Baseline: source.baseline, Mode: "baseline"}
	if work.AppliedSequence >= 0 {
		cursor.Mode, cursor.After, cursor.Sequence = "delta", work.AppliedSequence, work.AppliedSequence
	}
	if work.Cursor != "" {
		prior, err := decodeRepositoryViewSourceCursor(work.Cursor)
		if err != nil || prior.Checkpoint != checkpoint || prior.Floor != cursor.Floor || prior.Baseline != cursor.Baseline || prior.Mode != cursor.Mode || prior.After != cursor.After {
			return domain.ErrValidation
		}
		cursor = prior
	}
	if page.Mode != cursor.Mode {
		return domain.ErrValidation
	}
	pageBytes := 0
	for _, entry := range page.Entries {
		pageBytes += len(entry.Value) + len(entry.Key) + 128
		if len(entry.Value) > domain.MaxRepositoryViewSourceImageBytes || pageBytes > domain.MaxRepositoryViewSourcePageBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
		ordered := entry.Kind > cursor.Kind || (entry.Kind == cursor.Kind && entry.Key > cursor.Key)
		if page.Mode == "delta" {
			ordered = entry.Sequence > cursor.Sequence || (entry.Sequence == cursor.Sequence && ordered)
		}
		if !ordered || !s.spec.kindValid(entry.Kind) || entry.Key == "" ||
			len(entry.Key) > domain.MaxRepositoryViewSourceKeyBytes || entry.Sequence < 0 || entry.Sequence > page.Through ||
			(page.Mode == "delta" && entry.Sequence <= work.AppliedSequence) || (page.Mode == "baseline" && entry.Deleted) ||
			(entry.Deleted && len(entry.Value) != 0) || (!entry.Deleted && (!json.Valid(entry.Value) || len(bytes.TrimSpace(entry.Value)) == 0 || bytes.TrimSpace(entry.Value)[0] != '{')) {
			return domain.ErrValidation
		}
		cursor.Sequence, cursor.Kind, cursor.Key = entry.Sequence, entry.Kind, entry.Key
	}
	if page.NextCursor != "" {
		next, err := decodeRepositoryViewSourceCursor(page.NextCursor)
		if err != nil || page.Checkpoint != nil || len(page.Entries) == 0 || next != cursor || page.NextCursor == work.Cursor {
			return domain.ErrValidation
		}
	} else if page.Checkpoint == nil || *page.Checkpoint != checkpoint {
		return domain.ErrValidation
	}
	return nil
}

func (s projectionStore) savePage(ctx context.Context, lease domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
	return s.savePageWith(ctx, lease, page, nil)
}

func (s projectionStore) savePageWith(ctx context.Context, lease domain.PendingProjectionLease, page domain.RepositoryViewSourcePage, saveRelated func(pgx.Tx) error) error {
	if !s.leaseValid(lease) || lease.Phase != "capture" {
		return domain.ErrValidation
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if saveRelated != nil {
		if err := s.checkRepositoryDocumentIdentity(ctx, transaction, lease.RepoID, true); err != nil {
			return err
		}
	}
	work, source, err := s.lockLease(ctx, transaction, lease)
	if err != nil {
		return err
	}
	if err := s.validatePage(work, source, page); err != nil {
		return err
	}
	if saveRelated != nil {
		if err := saveRelated(transaction); err != nil {
			return err
		}
	}
	if work.AppliedSequence >= 0 && work.Cursor == "" && page.Mode == "delta" && len(page.Entries) == 0 && page.Checkpoint != nil {
		result, err := transaction.Exec(ctx, `UPDATE `+s.spec.ready+` SET source_seq=$5
			WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$6`,
			string(lease.RepoID), lease.Dataset, lease.Epoch, lease.AppliedSequence, page.Through, s.spec.rules)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return domain.ErrIntegrity
		}
		result, err = transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET phase='idle',applied_seq=$2,input_seq=$2,pin_seq=$2,cursor='',
			lease_until=NULL,attempts=0,reason='',next_attempt=clock_timestamp(),retention_until=clock_timestamp()+interval '24 hours'
			WHERE repo_id=$1 AND lease_until>clock_timestamp() AND retention_until>clock_timestamp()`, string(lease.RepoID), page.Through)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return domain.ErrRefConflict
		}
		return transaction.Commit(ctx)
	}
	if len(page.Entries) != 0 {
		raw, err := json.Marshal(page.Entries)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO `+s.spec.inputs+`(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
			SELECT $1,$2,entry.sequence,entry.kind,entry.key,COALESCE(entry.deleted,false),entry.value
			FROM jsonb_to_recordset($3::jsonb) AS entry(sequence bigint,kind text,key text,deleted boolean,value jsonb)`, string(lease.RepoID), lease.Dataset, raw); err != nil {
			return err
		}
		if _, err := transaction.Exec(ctx, `WITH changed AS (
			SELECT kind,key,max(sequence) AS sequence FROM jsonb_to_recordset($3::jsonb) AS entry(sequence bigint,kind text,key text)
			GROUP BY kind,key)
			DELETE FROM `+s.spec.inputs+` obsolete USING changed
			WHERE obsolete.repo_id=$1 AND obsolete.dataset=$2 AND obsolete.kind=changed.kind
			AND obsolete.entity_key=changed.key AND obsolete.source_seq<changed.sequence`, string(lease.RepoID), lease.Dataset, raw); err != nil {
			return err
		}
	}
	phase, input := "capture", int64(-1)
	if page.Checkpoint != nil {
		phase, input = "project", page.Through
		if _, err := transaction.Exec(ctx, `DELETE FROM `+s.spec.inputs+` WHERE repo_id=$1 AND dataset=$2 AND deleted`, string(lease.RepoID), lease.Dataset); err != nil {
			return err
		}
	}
	result, err := transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET phase=$2,input_seq=$3,cursor=$4,
		lease_until=NULL,attempts=0,reason='',next_attempt=clock_timestamp(),retention_until=clock_timestamp()+interval '24 hours'
		WHERE repo_id=$1 AND lease_until>clock_timestamp() AND retention_until>clock_timestamp()`, string(lease.RepoID), phase, input, page.NextCursor)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}

func (s projectionStore) loadInputs(ctx context.Context, lease domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	if !s.leaseValid(lease) || lease.Phase != "project" {
		return nil, domain.ErrValidation
	}
	var entries []domain.RepositoryViewSourceEntry
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, s.db(bound), lease.RepoID, false); err != nil {
			return err
		}
		work, err := scanProjectionWork(s.db(bound).QueryRow(bound, `SELECT `+projectionWorkColumns+`
			FROM `+s.spec.work+` work WHERE repo_id=$1`, string(lease.RepoID)))
		if err != nil {
			return err
		}
		if !projectionLeaseMatches(work, lease) {
			return domain.ErrRefConflict
		}
		source, err := s.readSource(bound, s.db(bound), lease.RepoID, false)
		if err != nil {
			return err
		}
		if source.epoch != lease.Epoch || source.head < lease.InputSequence || source.floor > work.pin {
			return domain.ErrCatalogResetRequired
		}
		var count, size, imageSize, keySize int64
		if err := s.db(bound).QueryRow(bound, `SELECT count(*),COALESCE(sum(octet_length(image::text)+octet_length(entity_key)+128),0),
			COALESCE(max(octet_length(image::text)),0),COALESCE(max(octet_length(entity_key)),0)
			FROM (SELECT * FROM (`+s.latestInputs()+`) latest WHERE NOT deleted LIMIT $4) active`,
			string(lease.RepoID), lease.Dataset, lease.InputSequence, domain.MaxPendingProjectionInputRows+1).Scan(&count, &size, &imageSize, &keySize); err != nil {
			return err
		}
		if count > domain.MaxPendingProjectionInputRows || size > domain.MaxPendingProjectionInputBytes ||
			imageSize > domain.MaxRepositoryViewSourceImageBytes || keySize > domain.MaxRepositoryViewSourceKeyBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
		rows, err := s.db(bound).Query(bound, `SELECT source_seq,kind,entity_key,image FROM (`+s.latestInputs()+`)
			latest WHERE NOT deleted ORDER BY kind,entity_key LIMIT $4`, string(lease.RepoID), lease.Dataset, lease.InputSequence, domain.MaxPendingProjectionInputRows+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		entries = make([]domain.RepositoryViewSourceEntry, 0, int(count))
		for rows.Next() {
			var entry domain.RepositoryViewSourceEntry
			if err := rows.Scan(&entry.Sequence, &entry.Kind, &entry.Key, &entry.Value); err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if int64(len(entries)) != count {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (s projectionStore) validateParts(ctx context.Context, repo domain.ContentHash, parts []domain.PendingProjectionPart) error {
	if len(parts) > domain.MaxPendingProjectionOutputRows {
		return domain.ErrRepositoryViewSourceLimit
	}
	size := 0
	seen := make(map[[2]string]bool, len(parts))
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		size += len(part.Value) + len(part.Kind) + len(part.Key) + 128
		if size > domain.MaxPendingProjectionOutputBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
		identity := [2]string{part.Kind, part.Key}
		if part.Kind == "" || len(part.Kind) > 128 || part.Key == "" || len(part.Key) > domain.MaxRepositoryViewSourceKeyBytes ||
			seen[identity] || !json.Valid(part.Value) || bytes.Equal(bytes.TrimSpace(part.Value), []byte("null")) {
			return domain.ErrValidation
		}
		seen[identity] = true
	}
	return s.spec.decodeParts(ctx, repo, parts)
}

func (s projectionStore) publish(ctx context.Context, lease domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
	if !s.leaseValid(lease) || lease.Phase != "project" {
		return domain.ErrValidation
	}
	if err := s.validateParts(ctx, lease.RepoID, parts); err != nil {
		return err
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, _, err := s.lockLease(ctx, transaction, lease); err != nil {
		return err
	}
	if lease.AppliedSequence >= 0 {
		var matches bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.spec.ready+`
			WHERE repo_id=$1 AND dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$5)`,
			string(lease.RepoID), lease.Dataset, lease.Epoch, lease.AppliedSequence, s.spec.rules).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return domain.ErrIntegrity
		}
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	var storedBytes int64
	if err := transaction.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(value::text)+octet_length(kind)+octet_length(key)+128),0)
		FROM jsonb_to_recordset($1::jsonb) AS part(kind text,key text,value jsonb)`, raw).Scan(&storedBytes); err != nil {
		return err
	}
	if storedBytes > domain.MaxPendingProjectionOutputBytes {
		return domain.ErrRepositoryViewSourceLimit
	}
	if _, err := transaction.Exec(ctx, `WITH latest AS MATERIALIZED (`+s.latestParts()+`),
		incoming AS MATERIALIZED (SELECT * FROM jsonb_to_recordset($4::jsonb) AS part(kind text,key text,value jsonb))
		INSERT INTO `+s.spec.parts+`(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		SELECT $1,$2,$5,COALESCE(incoming.kind,previous.kind),COALESCE(incoming.key,previous.entity_key),incoming.kind IS NULL,incoming.value
		FROM incoming FULL JOIN (SELECT * FROM latest WHERE NOT deleted) previous
		ON incoming.kind=previous.kind AND incoming.key=previous.entity_key
		WHERE incoming.value IS DISTINCT FROM previous.image`, string(lease.RepoID), lease.Dataset, lease.AppliedSequence, raw, lease.InputSequence); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM `+s.spec.parts+` obsolete USING `+s.spec.parts+` changed
		WHERE changed.repo_id=$1 AND changed.dataset=$2 AND changed.source_seq=$3
		AND obsolete.repo_id=changed.repo_id AND obsolete.dataset=changed.dataset
		AND obsolete.kind=changed.kind AND obsolete.entity_key=changed.entity_key AND obsolete.source_seq<changed.source_seq`,
		string(lease.RepoID), lease.Dataset, lease.InputSequence); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM `+s.spec.parts+` WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3 AND deleted`,
		string(lease.RepoID), lease.Dataset, lease.InputSequence); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO `+s.spec.ready+`(repo_id,dataset,source_epoch,source_seq,rules)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(repo_id) DO UPDATE SET dataset=EXCLUDED.dataset,source_epoch=EXCLUDED.source_epoch,
		source_seq=EXCLUDED.source_seq,rules=EXCLUDED.rules`, string(lease.RepoID), lease.Dataset, lease.Epoch, lease.InputSequence, s.spec.rules); err != nil {
		return err
	}
	if err := s.pruneDatasets(ctx, transaction, lease.RepoID); err != nil {
		return err
	}
	result, err := transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET phase='idle',applied_seq=input_seq,cursor='',pin_seq=input_seq,
		lease_until=NULL,attempts=0,reason='',next_attempt=clock_timestamp(),retention_until=clock_timestamp()+interval '24 hours'
		WHERE repo_id=$1 AND lease_until>clock_timestamp() AND retention_until>clock_timestamp()`, string(lease.RepoID))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}

func (s projectionStore) fail(ctx context.Context, lease domain.PendingProjectionLease, reason string, retry bool) error {
	if !s.leaseValid(lease) {
		return domain.ErrValidation
	}
	switch reason {
	case "source_reset", "lease_lost", "resource_limit", "invalid_source", "access_denied", "identity_upgrade_required", "temporary_failure":
	default:
		reason = "temporary_failure"
	}
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	work, _, err := s.lockLease(ctx, transaction, lease)
	if err != nil {
		return err
	}
	phase := work.Phase
	if !retry {
		phase = "needs_attention"
	}
	delay := min(int64(300), int64(1)<<min(max(work.Attempts-1, 0), 9))
	result, err := transaction.Exec(ctx, `UPDATE `+s.spec.work+` SET phase=$2,reason=$3,lease_until=NULL,
		next_attempt=clock_timestamp()+$4::bigint*interval '1 second' WHERE repo_id=$1 AND lease_until>clock_timestamp()
		AND retention_until>clock_timestamp()`, string(lease.RepoID), phase, reason, delay)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return transaction.Commit(ctx)
}

func (s projectionStore) readReadyParts(ctx context.Context, repo domain.ContentHash, dataset string, sequence int64) ([]domain.PendingProjectionPart, error) {
	if !s.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	return s.readReadyPartsFrom(ctx, s.db(ctx), repo, dataset, sequence)
}

type projectionPartsReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s projectionStore) readReadyPartsFrom(ctx context.Context, database projectionPartsReader, repo domain.ContentHash, dataset string, sequence int64) ([]domain.PendingProjectionPart, error) {
	var headerSequence, headerBytes int64
	var deleted bool
	err := database.QueryRow(ctx, `SELECT source_seq,deleted,COALESCE(octet_length(image::text),0)
		FROM `+s.spec.parts+` WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 AND kind='header' AND entity_key='view'
		ORDER BY source_seq DESC LIMIT 1`, string(repo), dataset, sequence).Scan(&headerSequence, &deleted, &headerBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntegrity
	}
	if err != nil {
		return nil, err
	}
	if deleted || headerBytes == 0 {
		return nil, domain.ErrIntegrity
	}
	headerBytes += int64(len("headerview") + 128)
	if headerBytes > domain.MaxPendingProjectionOutputBytes {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	header := domain.PendingProjectionPart{Kind: "header", Key: "view"}
	if err := database.QueryRow(ctx, `SELECT image FROM `+s.spec.parts+`
		WHERE repo_id=$1 AND dataset=$2 AND source_seq=$3 AND kind='header' AND entity_key='view'`, string(repo), dataset, headerSequence).Scan(&header.Value); err != nil {
		return nil, err
	}
	kinds, keys, err := s.spec.partKeys(header)
	if err != nil {
		return nil, err
	}
	parts := make([]domain.PendingProjectionPart, 0, 1+len(keys))
	parts = append(parts, header)
	if len(keys) == 0 {
		return parts, nil
	}
	var count, invalid, bodyBytes int64
	if err := database.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE latest.deleted OR latest.size=0),
		COALESCE(sum(latest.size+octet_length(wanted.kind)+octet_length(wanted.entity_key)+128),0)
		FROM unnest($4::text[],$5::text[]) AS wanted(kind,entity_key)
		JOIN LATERAL (SELECT deleted,COALESCE(octet_length(image::text),0) AS size FROM `+s.spec.parts+`
			WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 AND kind=wanted.kind AND entity_key=wanted.entity_key
			ORDER BY source_seq DESC LIMIT 1) latest ON true`, string(repo), dataset, sequence, kinds, keys).Scan(&count, &invalid, &bodyBytes); err != nil {
		return nil, err
	}
	if count != int64(len(keys)) || invalid != 0 {
		return nil, domain.ErrIntegrity
	}
	if bodyBytes+headerBytes > domain.MaxPendingProjectionOutputBytes {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	rows, err := database.Query(ctx, `SELECT wanted.kind,wanted.entity_key,latest.image
		FROM unnest($4::text[],$5::text[]) AS wanted(kind,entity_key)
		JOIN LATERAL (SELECT image FROM `+s.spec.parts+`
			WHERE repo_id=$1 AND dataset=$2 AND source_seq<=$3 AND kind=wanted.kind AND entity_key=wanted.entity_key
			ORDER BY source_seq DESC LIMIT 1) latest ON true`, string(repo), dataset, sequence, kinds, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var part domain.PendingProjectionPart
		if err := rows.Scan(&part.Kind, &part.Key, &part.Value); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(parts)) != count+1 {
		return nil, domain.ErrIntegrity
	}
	sort.Slice(parts, func(left, right int) bool {
		if parts[left].Kind != parts[right].Kind {
			return parts[left].Kind < parts[right].Kind
		}
		return parts[left].Key < parts[right].Key
	})
	return parts, nil
}

func (s projectionStore) read(ctx context.Context, repo domain.ContentHash, includeParts bool) (domain.PendingProjectionRead, error) {
	result, err := s.readWithOptions(ctx, repo, projectionReadOptions{includeParts: includeParts})
	return result.PendingProjectionRead, err
}

func (s projectionStore) readWithOptions(ctx context.Context, repo domain.ContentHash, options projectionReadOptions) (projectionRead, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return projectionRead{}, err
	}
	var result projectionRead
	outputFailed := false
	err := s.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !s.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := s.checkRepositoryDocumentIdentity(bound, s.db(bound), repo, false); err != nil {
			return err
		}
		source, err := s.readSource(bound, s.db(bound), repo, false)
		if err != nil {
			return err
		}
		result.Source = s.checkpoint(repo, source.epoch, source.head)
		result.Applied = s.checkpoint(repo, source.epoch, -1)
		var epoch string
		var sequence int64
		err = s.db(bound).QueryRow(bound, `SELECT work.enabled,work.phase='needs_attention',COALESCE(ready.dataset::text,''),
			COALESCE(ready.source_epoch::text,''),COALESCE(ready.source_seq,-1),COALESCE(ready.rules,0)
			FROM `+s.spec.work+` work LEFT JOIN `+s.spec.ready+` ready USING(repo_id) WHERE work.repo_id=$1`,
			string(repo)).Scan(&result.Enabled, &result.NeedsAttention, &result.Dataset, &epoch, &sequence, &result.rules)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if epoch != "" {
			result.Applied = s.checkpoint(repo, epoch, sequence)
		}
		result.complete = result.Enabled && !result.NeedsAttention && result.Dataset != "" &&
			epoch == source.epoch && sequence >= 0 && sequence <= source.head && result.rules == s.spec.rules
		result.Ready = result.complete && sequence == source.head
		if !options.includeParts || !result.complete || (!options.allowPrior && !result.Ready) {
			return nil
		}
		result.Parts, err = s.readReadyParts(bound, repo, result.Dataset, sequence)
		if err != nil {
			outputFailed = true
			return err
		}
		if err := s.validateParts(bound, repo, result.Parts); err != nil {
			outputFailed = true
			return err
		}
		return nil
	})
	if err != nil {
		if outputFailed && options.retainFailedOutput {
			result.Parts = nil
			return result, err
		}
		return projectionRead{}, err
	}
	return result, nil
}
