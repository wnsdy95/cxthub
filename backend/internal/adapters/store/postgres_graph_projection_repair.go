//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphProjectionRepairStore = (*PostgresStore)(nil)

func graphProjectionOutputCorruption(ctx context.Context, repo domain.ContentHash, read projectionRead, cause error) *domain.GraphProjectionCorruption {
	var databaseError *pgconn.PgError
	if cause == nil || ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) ||
		errors.As(cause, &databaseError) || !read.complete || !read.Enabled || read.NeedsAttention ||
		read.Source.Validate() != nil || read.Source.Scope != domain.RepositoryGraphSourceScope || read.Source.RepoID != repo ||
		read.Applied.Epoch != read.Source.Epoch || read.Applied.Sequence > read.Source.Sequence {
		return nil
	}
	failure := &domain.GraphProjectionCorruption{RepoID: repo, Dataset: read.Dataset, Applied: read.Applied, Rules: read.rules, Cause: cause}
	if failure.Validate() != nil {
		return nil
	}
	return failure
}

func graphProjectionRepairQuarantined(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash) (bool, error) {
	var quarantined bool
	err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph_projection_repairs WHERE repo_id=$1 AND quarantined)`, string(repo)).Scan(&quarantined)
	return quarantined, err
}

func graphProjectionRepairWindow(rebuilds []time.Time, now time.Time) ([]time.Time, bool) {
	active := make([]time.Time, 0, 3)
	for _, rebuilt := range rebuilds {
		if rebuilt.After(now.Add(-time.Hour)) {
			active = append(active, rebuilt)
		}
	}
	if len(active) >= 3 {
		return active, true
	}
	return append(active, now), false
}

func graphProjectionRepairReason(cause error) string {
	if errors.Is(cause, domain.ErrRepositoryViewSourceLimit) {
		return "resource_limit"
	}
	if errors.Is(cause, domain.ErrIntegrity) {
		return "integrity"
	}
	return "validation"
}

func (s *PostgresStore) RequestGraphProjectionRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.InTransaction(ctx) || outbound.DocReadOnly(ctx) {
		return domain.ErrConflict
	}
	if err := failure.Validate(); err != nil {
		return err
	}
	if failure.Serving {
		return domain.ErrValidation
	}
	projection := s.graphProjection()
	transaction, err := projection.begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	var required domain.DocumentIdentity
	if err := transaction.QueryRow(ctx, `SELECT required_doc_identity FROM repos WHERE id=$1 FOR SHARE`, string(failure.RepoID)).Scan(&required); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	work, err := scanProjectionWork(transaction.QueryRow(ctx, `SELECT `+projectionWorkColumns+`
		FROM graph_projection_work work WHERE repo_id=$1 FOR UPDATE`, string(failure.RepoID)))
	if errors.Is(err, domain.ErrNotFound) || (err == nil && !work.enabled) {
		return nil
	}
	if err != nil {
		return err
	}
	source, err := projection.readSource(ctx, transaction, failure.RepoID, true)
	if err != nil {
		return err
	}
	if source.epoch != failure.Applied.Epoch || source.head < failure.Applied.Sequence || source.baseline > failure.Applied.Sequence {
		return nil
	}
	var dataset, epoch string
	var sequence int64
	var rules int
	err = transaction.QueryRow(ctx, `SELECT dataset::text,source_epoch::text,source_seq,rules
		FROM graph_projection_ready WHERE repo_id=$1`, string(failure.RepoID)).Scan(&dataset, &epoch, &sequence, &rules)
	missingReady := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missingReady {
		return err
	}
	if !missingReady && (dataset != failure.Dataset || epoch != failure.Applied.Epoch || sequence != failure.Applied.Sequence || rules != failure.Rules) {
		return nil
	}
	var rebuilds []time.Time
	var quarantined, sameReceipt bool
	err = transaction.QueryRow(ctx, `SELECT rebuilds,quarantined,
		dataset=$2 AND source_epoch=$3 AND source_seq=$4 AND rules=$5
		FROM graph_projection_repairs WHERE repo_id=$1`, string(failure.RepoID), failure.Dataset, failure.Applied.Epoch, failure.Applied.Sequence, failure.Rules).
		Scan(&rebuilds, &quarantined, &sameReceipt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if (missingReady || work.Phase == "needs_attention") && !sameReceipt {
		return nil
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, required); err != nil {
		return err
	}
	if sameReceipt || quarantined {
		_, err := transaction.Exec(ctx, `UPDATE graph_projection_repairs
			SET request_count=LEAST(request_count,9223372036854775806)+1,last_requested_at=clock_timestamp() WHERE repo_id=$1`, string(failure.RepoID))
		if err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	rebuilds, quarantined = graphProjectionRepairWindow(rebuilds, now)
	_, err = transaction.Exec(ctx, `INSERT INTO graph_projection_repairs
		(repo_id,dataset,source_epoch,source_seq,rules,reason,request_count,last_requested_at,window_started_at,rebuilds,quarantined)
		VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10)
		ON CONFLICT(repo_id) DO UPDATE SET dataset=EXCLUDED.dataset,source_epoch=EXCLUDED.source_epoch,
		source_seq=EXCLUDED.source_seq,rules=EXCLUDED.rules,reason=EXCLUDED.reason,
		request_count=LEAST(graph_projection_repairs.request_count,9223372036854775806)+1,
		last_requested_at=EXCLUDED.last_requested_at,window_started_at=EXCLUDED.window_started_at,
		rebuilds=EXCLUDED.rebuilds,quarantined=EXCLUDED.quarantined`,
		string(failure.RepoID), failure.Dataset, failure.Applied.Epoch, failure.Applied.Sequence, failure.Rules,
		graphProjectionRepairReason(failure.Cause), now, rebuilds[0], rebuilds, quarantined)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM graph_projection_ready WHERE repo_id=$1`, string(failure.RepoID)); err != nil {
		return err
	}
	phase, reason := "capture", ""
	if quarantined {
		phase, reason = "needs_attention", "invalid_source"
	}
	_, err = transaction.Exec(ctx, `UPDATE graph_projection_work SET source_epoch=$2,dataset=gen_random_uuid(),
		applied_seq=-1,input_seq=-1,cursor='',phase=$3,lease_until=NULL,fence=fence+1,attempts=0,
		next_attempt=clock_timestamp(),reason=$4,pin_seq=$5,retention_until=clock_timestamp()+interval '24 hours' WHERE repo_id=$1`,
		string(failure.RepoID), source.epoch, phase, reason, source.floor)
	if err != nil {
		return err
	}
	return transaction.Commit(ctx)
}
