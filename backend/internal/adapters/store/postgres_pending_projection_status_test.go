//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pendingProjectionStatusTx struct {
	pgx.Tx
	queries []*repositoryViewSourceBudgetQuery
}

func (observed *pendingProjectionStatusTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	for _, forbidden := range []string{
		"pending_projection_parts", "pending_projection_inputs", "repository_view_source_images",
		"repository_view_source_changes", "repository_catalog_changes", "snapshots", "pending_contexts",
	} {
		if strings.Contains(strings.ToLower(statement), forbidden) {
			return nil, fmt.Errorf("status read accessed payload relation %s: %s", forbidden, statement)
		}
	}
	rows, err := observed.Tx.Query(ctx, statement, args...)
	if err != nil {
		return rows, err
	}
	budget := &repositoryViewSourceBudgetQuery{columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			budget.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, budget)
	return &repositoryViewSourceBudgetRows{Rows: rows, record: budget}, nil
}

func (observed *pendingProjectionStatusTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	rows, err := observed.Query(ctx, statement, args...)
	return pendingProjectionReadCostRow{rows: rows, err: err}
}

type pendingProjectionStatusCost struct {
	queries int
	rows    int
	columns int
	bytes   int
}

func pendingProjectionStatusObservedPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) (domain.PendingProjectionRead, pendingProjectionStatusCost) {
	t.Helper()
	var read domain.PendingProjectionRead
	var cost pendingProjectionStatusCost
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
		observed := &pendingProjectionStatusTx{Tx: transaction.Tx}
		transaction.Tx = observed
		var err error
		read, err = storage.ReadPendingProjectionStatus(context.WithValue(bound, repositoryTxKey{}, &transaction), repo)
		if err != nil {
			return err
		}
		for _, query := range observed.queries {
			if query.jsonColumns != 0 || query.images != 0 || query.imageBytes != 0 || query.rows > 1 {
				t.Fatalf("status transferred payload or non-point rows: %+v", query)
			}
			cost.queries++
			cost.rows += query.rows
			cost.columns += query.columns
			cost.bytes += query.valueBytes
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read.Parts != nil || cost.queries != 3 || cost.rows > 3 || cost.columns > 12 || cost.bytes > 512 {
		t.Fatalf("status exceeded metadata-only budget: read=%+v cost=%+v", read, cost)
	}
	return read, cost
}

func pendingProjectionStatusReadyPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.PendingProjectionRead {
	t.Helper()
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	return pendingProjectionReadPG(t, ctx, storage, job, view)
}

func TestPGPendingProjectionStatusBeforePublication(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	before, _ := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
	if !before.Enabled || before.Ready || before.NeedsAttention || before.Dataset != "" || before.Applied.Sequence != -1 || before.Applied.Epoch != before.Source.Epoch {
		t.Fatalf("uncaptured status exposed ready metadata: %+v", before)
	}
	pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	after, _ := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("unpublished capture changed ready metadata: before=%+v after=%+v", before, after)
	}
}

func TestPGPendingProjectionStatusStates(t *testing.T) {
	for _, state := range []string{"ready", "catching_up", "needs_attention", "reset", "disabled", "rules", "missing_ready", "missing_work"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, repo, _ := pendingProjectionPG(t)
			before := pendingProjectionStatusReadyPG(t, ctx, storage, repo)
			switch state {
			case "catching_up":
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"catching up"') WHERE repo_id=$1`, string(repo))
			case "needs_attention":
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_work SET phase='needs_attention',reason='resource_limit' WHERE repo_id=$1`, string(repo))
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "disabled":
				if err := storage.ConfigurePendingProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_ready SET rules=rules+1 WHERE repo_id=$1`, string(repo))
			case "missing_ready":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM pending_projection_ready WHERE repo_id=$1`, string(repo))
			case "missing_work":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM pending_projection_work WHERE repo_id=$1`, string(repo))
			}
			status, _ := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
			if status.Ready != (state == "ready") || status.Enabled != (state != "disabled" && state != "missing_work") || status.NeedsAttention != (state == "needs_attention") {
				t.Fatalf("incorrect readiness policy for %s: %+v", state, status)
			}
			source := catalogReadStatePG(t, ctx, storage, repo)
			if status.Source != pendingProjectionCheckpoint(repo, source.epoch, source.head) {
				t.Fatalf("status lost current source: %+v source=%+v", status, source)
			}
			if state == "missing_ready" || state == "missing_work" {
				if status.Dataset != "" || status.Applied != pendingProjectionCheckpoint(repo, source.epoch, -1) {
					t.Fatalf("missing publication exposed applied metadata: %+v", status)
				}
			} else if status.Dataset != before.Dataset || status.Applied != before.Applied {
				t.Fatalf("status changed retained publication: before=%+v after=%+v", before, status)
			}
			if state != "ready" && state != "missing_work" {
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_parts SET image='{"invalid":"unreadable"}'::jsonb WHERE repo_id=$1 AND NOT deleted`, string(repo))
			}
			full, err := storage.ReadPendingProjection(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if status.Ready != (len(full.Parts) > 0) {
				t.Fatalf("full read loaded parts outside ready state: %+v", full)
			}
			full.Parts = nil
			if !reflect.DeepEqual(full, status) {
				t.Fatalf("full and metadata reads disagree: full=%+v status=%+v", full, status)
			}
		})
	}
}

func TestPGPendingProjectionStatusSourceOnlyMutationSameRevisions(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	pendingProjectionStatusReadyPG(t, ctx, storage, repo)
	before, _ := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
	revision, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		pinned, _ := pendingProjectionStatusObservedPG(t, bound, storage, repo)
		catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"source-only mutation"') WHERE repo_id=$1`, string(repo))
		currentRevision, err := storage.RepositoryRevision(ctx, repo)
		if err != nil || currentRevision != revision {
			t.Fatalf("source-only mutation changed G/P/E: before=%+v after=%+v err=%v", revision, currentRevision, err)
		}
		fresh, _ := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
		if fresh.Ready || !fresh.Enabled || fresh.NeedsAttention || fresh.Source.Epoch != before.Source.Epoch || fresh.Source.Sequence <= before.Source.Sequence || fresh.Applied != before.Applied || fresh.Dataset != before.Dataset {
			t.Fatalf("source-only mutation did not invalidate status: before=%+v fresh=%+v", before, fresh)
		}
		stillPinned, _ := pendingProjectionStatusObservedPG(t, bound, storage, repo)
		if !reflect.DeepEqual(pinned, before) || !reflect.DeepEqual(stillPinned, before) {
			t.Fatalf("status mixed read snapshots: before=%+v pinned=%+v after=%+v", before, pinned, stillPinned)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGPendingProjectionStatusTransferIndependentOfHistoricalPayloads(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	pendingProjectionStatusReadyPG(t, ctx, storage, repo)
	before, beforeCost := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
	for _, count := range []int{1, 4096} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			for _, table := range []string{"pending_projection_inputs", "pending_projection_parts"} {
				catalogExecPG(t, ctx, storage.pool, `INSERT INTO `+table+`(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
					SELECT $1,$2,0,'pending','status-history-'||position::text,false,jsonb_build_object('poison',repeat('x',2500))
					FROM generate_series(1,$3::int) position ON CONFLICT DO NOTHING`, string(repo), before.Dataset, count)
			}
			catalogExecPG(t, ctx, storage.pool, `INSERT INTO repository_view_source_changes(repo_id,epoch,seq,entity_kind,entity_key,deleted,after_image)
				SELECT $1,$2,0,'pending','status-history-'||position::text,false,jsonb_build_object('poison',repeat('x',2500))
				FROM generate_series(1,$3::int) position ON CONFLICT DO NOTHING`, string(repo), before.Source.Epoch, count)
			var payloadRows int
			var payloadBytes int64
			if err := storage.pool.QueryRow(ctx, `SELECT count(*),sum(octet_length(image::text)) FROM (
				SELECT image FROM pending_projection_inputs WHERE repo_id=$1 AND source_seq=0 AND entity_key LIKE 'status-history-%'
				UNION ALL SELECT image FROM pending_projection_parts WHERE repo_id=$1 AND source_seq=0 AND entity_key LIKE 'status-history-%'
				UNION ALL SELECT after_image FROM repository_view_source_images WHERE repo_id=$1 AND seq=0 AND entity_key LIKE 'status-history-%'
				) payloads`, string(repo)).Scan(&payloadRows, &payloadBytes); err != nil || payloadRows != count*3 || payloadBytes < int64(count*3*2500) {
				t.Fatalf("historical payload fixture missing: rows=%d bytes=%d err=%v", payloadRows, payloadBytes, err)
			}
			after, afterCost := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
			if !reflect.DeepEqual(after, before) || afterCost != beforeCost {
				t.Fatalf("history changed status or transfer: before=%+v after=%+v cost=%+v want=%+v", before, after, afterCost, beforeCost)
			}
		})
	}
	for _, state := range []string{"invalid", "oversized", "missing"} {
		t.Run(state, func(t *testing.T) {
			want := domain.ErrIntegrity
			switch state {
			case "invalid":
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_parts SET image='{"invalid":true}'::jsonb WHERE repo_id=$1 AND dataset=$2 AND kind='header'`, string(repo), before.Dataset)
			case "oversized":
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_projection_parts SET image=jsonb_build_object('oversized',repeat('x',$3::int)) WHERE repo_id=$1 AND dataset=$2 AND kind='header'`, string(repo), before.Dataset, domain.MaxPendingProjectionOutputBytes)
				want = domain.ErrRepositoryViewSourceLimit
			case "missing":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM pending_projection_parts WHERE repo_id=$1`, string(repo))
			}
			status, cost := pendingProjectionStatusObservedPG(t, ctx, storage, repo)
			if !reflect.DeepEqual(status, before) || cost != beforeCost {
				t.Fatalf("payload validity changed metadata readiness: status=%+v cost=%+v", status, cost)
			}
			full, err := storage.ReadPendingProjection(ctx, repo)
			if !errors.Is(err, want) || !reflect.DeepEqual(full, domain.PendingProjectionRead{}) {
				t.Fatalf("full read skipped payload validation: read=%+v err=%v want=%v", full, err, want)
			}
		})
	}
}

func TestPGPendingProjectionStatusIdentityAndRepository(t *testing.T) {
	storage, ctx, repo, _ := pendingProjectionPG(t)
	pendingProjectionStatusReadyPG(t, ctx, storage, repo)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before, _ := pendingProjectionStatusObservedPG(t, bound, storage, repo)
		if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
			return err
		}
		after, _ := pendingProjectionStatusObservedPG(t, bound, storage, repo)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("identity requirement crossed owned snapshot: before=%+v after=%+v", before, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, compatibility := range []struct {
		name         string
		peer, binary []domain.DocumentIdentity
	}{
		{name: "undeclared"},
		{name: "peer_only", peer: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}},
		{name: "binary_only", binary: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}},
	} {
		t.Run(compatibility.name, func(t *testing.T) {
			bound := outbound.WithDocumentIdentityCompatibility(ctx, compatibility.peer, compatibility.binary)
			for _, read := range []func(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error){storage.ReadPendingProjectionStatus, storage.ReadPendingProjection} {
				status, err := read(bound, repo)
				if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(status, domain.PendingProjectionRead{}) {
					t.Fatalf("incompatible identity received metadata: status=%+v err=%v", status, err)
				}
			}
		})
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	status, _ := pendingProjectionStatusObservedPG(t, compatible, storage, repo)
	if !status.Enabled || status.Ready || status.Source.Sequence <= status.Applied.Sequence {
		t.Fatalf("compatible reader lost current identity mutation: %+v", status)
	}
	for _, repository := range []struct {
		repo domain.ContentHash
		want error
	}{
		{repo: "invalid", want: domain.ErrIntegrity},
		{repo: domain.HashContent([]byte(t.Name() + "-missing")), want: domain.ErrNotFound},
	} {
		read, err := storage.ReadPendingProjectionStatus(compatible, repository.repo)
		if !errors.Is(err, repository.want) || !reflect.DeepEqual(read, domain.PendingProjectionRead{}) {
			t.Fatalf("invalid or missing repository returned metadata: %+v err=%v", read, err)
		}
	}
}

func TestPendingProjectionStatusRejectsUnownedAndWriteTransactions(t *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(t.Name()))
	for _, transaction := range []*repositoryTx{
		{owner: storage, repo: repo},
		{owner: &PostgresStore{}, repo: repo},
		{owner: &PostgresStore{}, readOnly: true},
	} {
		ctx := context.WithValue(context.Background(), repositoryTxKey{}, transaction)
		read, err := storage.ReadPendingProjectionStatus(ctx, repo)
		if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(read, domain.PendingProjectionRead{}) {
			t.Fatalf("status escaped caller transaction: read=%+v err=%v", read, err)
		}
	}
}
