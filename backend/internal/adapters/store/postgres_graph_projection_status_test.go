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

type graphProjectionStatusTx struct {
	pgx.Tx
	queries []*repositoryViewSourceBudgetQuery
}

func (observed *graphProjectionStatusTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	for _, forbidden := range []string{
		"graph_projection_parts", "graph_projection_inputs", "repository_view_source_images", "repository_graph_source_images",
		"repository_view_source_changes", "repository_graph_source_changes", "repository_catalog_changes", "snapshots", "pending_contexts",
	} {
		if strings.Contains(strings.ToLower(statement), forbidden) {
			return nil, fmt.Errorf("graph status accessed payload relation %s: %s", forbidden, statement)
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

func (observed *graphProjectionStatusTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	rows, err := observed.Query(ctx, statement, args...)
	return pendingProjectionReadCostRow{rows: rows, err: err}
}

func graphProjectionStatusObservedPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionRead {
	t.Helper()
	var result domain.GraphProjectionRead
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
		observed := &graphProjectionStatusTx{Tx: transaction.Tx}
		transaction.Tx = observed
		var err error
		result, err = storage.ReadGraphProjectionStatus(context.WithValue(bound, repositoryTxKey{}, &transaction), repo)
		if err != nil {
			return err
		}
		var columns, transferred int
		for _, query := range observed.queries {
			if query.jsonColumns != 0 || query.images != 0 || query.imageBytes != 0 || query.rows > 1 {
				t.Fatalf("graph status read transferred payloads or scanned rows: %+v", query)
			}
			columns += query.columns
			transferred += query.valueBytes
		}
		if len(observed.queries) != 3 || columns > 12 || transferred > 512 || result.Parts != nil {
			t.Fatalf("graph status exceeded fixed metadata budget: queries=%d columns=%d bytes=%d result=%+v", len(observed.queries), columns, transferred, result)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPGGraphProjectionStatusGenerationAvailability(t *testing.T) {
	for _, state := range []string{"ready", "lagged", "needs_attention", "reset", "disabled", "old_rules", "future_rules", "future_sequence", "missing_ready", "missing_work"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			graphProjectionPublishPG(t, ctx, storage, job)
			switch state {
			case "lagged":
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='lagged bundle' WHERE repo_id=$1`, string(repo))
			case "needs_attention":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET phase='needs_attention',reason='invalid_source' WHERE repo_id=$1`, string(repo))
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "disabled":
				if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "old_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=$2 WHERE repo_id=$1`, string(repo), domain.GraphProjectionRules)
			case "future_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=$2 WHERE repo_id=$1`, string(repo), domain.GraphViewProjectionRules+1)
			case "future_sequence":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET source_seq=source_seq+1 WHERE repo_id=$1`, string(repo))
			case "missing_ready":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_ready WHERE repo_id=$1`, string(repo))
			case "missing_work":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_work WHERE repo_id=$1`, string(repo))
			}
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			status := graphProjectionStatusObservedPG(t, ctx, storage, repo)
			complete := state == "ready" || state == "lagged"
			if status.Ready != (state == "ready") || status.Complete != complete || status.NeedsAttention != (state == "needs_attention") ||
				status.Enabled != (state != "disabled" && state != "missing_work") {
				t.Fatalf("incorrect graph generation availability: %+v", status)
			}
			rules := domain.GraphViewProjectionRules
			switch state {
			case "old_rules":
				rules = domain.GraphProjectionRules
			case "future_rules":
				rules++
			case "missing_ready", "missing_work":
				rules = 0
			}
			if status.Rules != rules {
				t.Fatalf("status relabeled stored rules: %+v want=%d", status, rules)
			}
			read, err := storage.ReadGraphProjection(ctx, repo)
			if err != nil || (len(read.Parts) > 0) != complete {
				t.Fatalf("bundle exposure disagrees with availability: %+v err=%v", read, err)
			}
			read.Parts = nil
			if !reflect.DeepEqual(read, status) {
				t.Fatalf("full and metadata-only reads disagree: full=%+v status=%+v", read, status)
			}
			if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
				t.Fatal("reading graph generation mutated durable state")
			}
		})
	}
}

func TestPGGraphProjectionIdentityRequirementFencesPriorBundleReads(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before := graphProjectionStatusObservedPG(t, bound, storage, repo)
		if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
			return err
		}
		after := graphProjectionStatusObservedPG(t, bound, storage, repo)
		if !reflect.DeepEqual(after, before) {
			t.Fatal("identity requirement crossed the pinned graph snapshot")
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
			for _, read := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadGraphProjection, storage.ReadGraphProjectionStatus} {
				result, err := read(bound, repo)
				if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(result, domain.GraphProjectionRead{}) {
					t.Fatalf("incompatible identity received prior graph metadata: %+v err=%v", result, err)
				}
			}
		})
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	status := graphProjectionStatusObservedPG(t, compatible, storage, repo)
	if !status.Enabled || !status.Complete || status.Ready || status.Source.Sequence <= status.Applied.Sequence {
		t.Fatalf("compatible reader lost prior bundle after identity mutation: %+v", status)
	}
	for _, repository := range []struct {
		repo domain.ContentHash
		want error
	}{
		{repo: "invalid", want: domain.ErrIntegrity},
		{repo: domain.HashContent([]byte(t.Name() + "-missing")), want: domain.ErrNotFound},
	} {
		for _, read := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadGraphProjection, storage.ReadGraphProjectionStatus} {
			result, err := read(compatible, repository.repo)
			if !errors.Is(err, repository.want) || !reflect.DeepEqual(result, domain.GraphProjectionRead{}) {
				t.Fatalf("invalid or missing repository received graph metadata: %+v err=%v", result, err)
			}
		}
	}
}

func TestGraphProjectionReadsRejectUnownedAndWriteTransactions(t *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(t.Name()))
	for _, transaction := range []*repositoryTx{
		{owner: storage, repo: repo},
		{owner: &PostgresStore{}, repo: repo},
		{owner: &PostgresStore{}, readOnly: true},
	} {
		ctx := context.WithValue(context.Background(), repositoryTxKey{}, transaction)
		for _, read := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadGraphProjection, storage.ReadGraphProjectionStatus} {
			result, err := read(ctx, repo)
			if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.GraphProjectionRead{}) {
				t.Fatalf("graph read escaped caller transaction: %+v err=%v", result, err)
			}
		}
	}
}
