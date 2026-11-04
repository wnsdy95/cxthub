//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphProjectionRepairLedger struct {
	dataset, epoch, reason string
	sequence, requests     int64
	rules                  int
	rebuilds               []time.Time
	window, requested      time.Time
	quarantined            bool
}

func graphProjectionRepairLedgerPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) graphProjectionRepairLedger {
	t.Helper()
	var ledger graphProjectionRepairLedger
	err := storage.pool.QueryRow(ctx, `SELECT dataset::text,source_epoch::text,source_seq,rules,reason,request_count,
		rebuilds,window_started_at,last_requested_at,quarantined FROM graph_projection_repairs WHERE repo_id=$1`, string(repo)).
		Scan(&ledger.dataset, &ledger.epoch, &ledger.sequence, &ledger.rules, &ledger.reason, &ledger.requests,
			&ledger.rebuilds, &ledger.window, &ledger.requested, &ledger.quarantined)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return ledger
}

func graphProjectionCorruptPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionCorruption {
	t.Helper()
	before, err := storage.ReadGraphProjectionStatus(ctx, repo)
	if err != nil || !before.Complete {
		t.Fatalf("missing complete corruption fixture: %+v %v", before, err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image='{}' WHERE repo_id=$1 AND dataset=$2 AND kind='header'`, string(repo), before.Dataset)
	read, err := storage.ReadGraphProjection(ctx, repo)
	var failure *domain.GraphProjectionCorruption
	if !errors.As(err, &failure) || failure.Validate() != nil || !reflect.DeepEqual(read, before) ||
		failure.Dataset != before.Dataset || failure.Applied != before.Applied || failure.Rules != before.Rules {
		t.Fatalf("corruption lost exact metadata: read=%+v before=%+v failure=%v", read, before, err)
	}
	return *failure
}

func graphProjectionRepairSourcePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) [][]byte {
	t.Helper()
	var result [][]byte
	for _, table := range []string{"repos", "repository_catalog_state", "repository_catalog_changes", "repository_view_source_state",
		"repository_graph_source_state", "repository_graph_source_changes", "snapshots", "refs", "reflog", "context_history", "pending_contexts", "repo_blobs"} {
		column := "repo_id"
		if table == "repos" {
			column = "id"
		}
		var rows []byte
		if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(source) || jsonb_build_object('xmin',source.xmin::text)
			ORDER BY to_jsonb(source)::text),'[]'::jsonb) FROM `+table+` source WHERE `+column+`=$1`, string(repo)).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		result = append(result, rows)
	}
	var documents []byte
	if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(blob) || jsonb_build_object('xmin',blob.xmin::text) ORDER BY blob.hash),'[]'::jsonb)
		FROM blobs blob WHERE EXISTS(SELECT 1 FROM repo_blobs owned WHERE owned.repo_id=$1 AND owned.hash=blob.hash)`, string(repo)).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	return append(result, documents)
}

func TestPGGraphProjectionRepairCoalescesFencesAndRecovers(t *testing.T) {
	for _, state := range []string{"idle", "capture", "project"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			var snapshotID domain.ContentHash
			if err := storage.pool.QueryRow(ctx, `SELECT id FROM snapshots WHERE repo_id=$1 LIMIT 1`, string(repo)).Scan(&snapshotID); err != nil {
				t.Fatal(err)
			}
			if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "repair-source", Target: snapshotID}, ""); err != nil {
				t.Fatal(err)
			}
			event := catalogHistoryPG(repo)
			eventJSON, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, storage.pool, `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, string(repo), event.ID, eventJSON)
			initial := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			graphProjectionPublishPG(t, ctx, storage, initial)
			var active domain.GraphProjectionLease
			var activePage domain.RepositoryViewSourcePage
			var activeParts []domain.GraphProjectionPart
			if state != "idle" {
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='lag before corrupt repair' WHERE repo_id=$1`, string(repo))
				active = graphProjectionClaimPG(t, ctx, storage, repo)
				if state == "project" {
					active = graphProjectionCapturePG(t, ctx, storage, active, 0)
					_, activeParts = graphProjectionPartsPG(t, ctx, storage, active)
				} else {
					activePage = graphProjectionPagePG(t, ctx, storage, active, 1)
				}
			}
			failure := graphProjectionCorruptPG(t, ctx, storage, repo)
			sourceBefore := graphProjectionRepairSourcePG(t, ctx, storage, repo)
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			workBefore, err := scanProjectionWork(storage.pool.QueryRow(ctx, `SELECT `+projectionWorkColumns+` FROM graph_projection_work work WHERE repo_id=$1`, string(repo)))
			if err != nil {
				t.Fatal(err)
			}
			const requests = 8
			results := make(chan error, requests)
			for request := 0; request < requests; request++ {
				go func() { results <- storage.RequestGraphProjectionRepair(ctx, failure) }()
			}
			for request := 0; request < requests; request++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			ledger := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
			if ledger.requests != requests || len(ledger.rebuilds) != 1 || ledger.quarantined || ledger.dataset != failure.Dataset ||
				ledger.epoch != failure.Applied.Epoch || ledger.sequence != failure.Applied.Sequence || ledger.rules != failure.Rules || ledger.reason != "integrity" {
				t.Fatalf("repair did not coalesce: %+v", ledger)
			}
			after := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if !bytes.Equal(after.inputs, before.inputs) || !bytes.Equal(after.parts, before.parts) || len(after.ready) != 0 ||
				!reflect.DeepEqual(graphProjectionRepairSourcePG(t, ctx, storage, repo), sourceBefore) {
				t.Fatal("repair rewrote immutable source or scanned/deleted derived payloads")
			}
			workAfter, err := scanProjectionWork(storage.pool.QueryRow(ctx, `SELECT `+projectionWorkColumns+` FROM graph_projection_work work WHERE repo_id=$1`, string(repo)))
			if err != nil || workAfter.Dataset == workBefore.Dataset || workAfter.Epoch != failure.Applied.Epoch || workAfter.Fence != workBefore.Fence+1 ||
				workAfter.Phase != "capture" || workAfter.AppliedSequence != -1 || workAfter.InputSequence != -1 || workAfter.Cursor != "" || workAfter.leased || !workAfter.retained {
				t.Fatalf("repair did not fence and reset capture: %+v %v", workAfter, err)
			}
			status, err := storage.ReadGraphProjectionStatus(ctx, repo)
			if err != nil || status.Complete || status.Ready || status.NeedsAttention || !status.Enabled {
				t.Fatalf("invalidated output remained serveable: %+v %v", status, err)
			}
			if state != "idle" {
				for _, operation := range []func() error{
					func() error { return storage.RenewGraphProjection(ctx, active, time.Minute) },
					func() error { return storage.FailGraphProjection(ctx, active, "invalid_source", false) },
				} {
					if err := operation(); !errors.Is(err, domain.ErrRefConflict) {
						t.Fatalf("old worker escaped fence: %v", err)
					}
				}
				if state == "capture" {
					err = storage.SaveGraphProjectionPage(ctx, active, activePage)
				} else {
					err = storage.PublishGraphProjection(ctx, active, activeParts)
				}
				if !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("old worker published after repair: %v", err)
				}
			}
			restarted, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if err := restarted.ConfigureGraphProjection(ctx, repo, true); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(graphProjectionSnapshotPG(t, ctx, storage, repo), after) {
				t.Fatal("restart discarded repair capture")
			}
			fresh := graphProjectionClaimPG(t, ctx, restarted, repo)
			if fresh.Dataset != workAfter.Dataset || fresh.AppliedSequence != -1 {
				t.Fatalf("repair did not resume fresh dataset: %+v", fresh)
			}
			fresh = graphProjectionCapturePG(t, ctx, restarted, fresh, 1)
			graphProjectionPublishPG(t, ctx, restarted, fresh)
			published := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(published, graphProjectionSnapshotPG(t, ctx, storage, repo)) ||
				!reflect.DeepEqual(ledger, graphProjectionRepairLedgerPG(t, ctx, storage, repo)) ||
				!reflect.DeepEqual(sourceBefore, graphProjectionRepairSourcePG(t, ctx, storage, repo)) {
				t.Fatal("stale receipt changed new ready output, ledger, or source")
			}
		})
	}
}

func TestPGGraphProjectionRepairStaleDisabledAndResetNoOps(t *testing.T) {
	for _, state := range []string{"dataset", "epoch", "sequence", "rules", "disabled", "reset", "baseline", "missing_ready", "missing_work"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			graphProjectionPublishPG(t, ctx, storage, job)
			failure := graphProjectionCorruptPG(t, ctx, storage, repo)
			switch state {
			case "dataset":
				failure.Dataset = "33333333-3333-4333-8333-333333333333"
			case "epoch":
				failure.Applied.Epoch = "33333333-3333-4333-8333-333333333333"
			case "sequence":
				failure.Applied.Sequence--
			case "rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=1 WHERE repo_id=$1`, string(repo))
			case "disabled":
				if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "baseline":
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='baseline reset' WHERE repo_id=$1`, string(repo))
				catalogExecPG(t, ctx, storage.pool, `UPDATE repository_graph_source_state source SET baseline_seq=catalog.head_seq
					FROM repository_catalog_state catalog WHERE source.repo_id=$1 AND catalog.repo_id=source.repo_id`, string(repo))
			case "missing_ready":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_ready WHERE repo_id=$1`, string(repo))
			case "missing_work":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_work WHERE repo_id=$1`, string(repo))
			}
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || graphProjectionRepairLedgerPG(t, ctx, storage, repo).requests != 0 {
				t.Fatal("stale or disabled receipt mutated repair state")
			}
		})
	}
}

func TestPGGraphProjectionRepairPreservesConcurrentTerminalFailure(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		for _, phase := range []string{"capture", "project"} {
			t.Run(phase+map[bool]string{false: "_delayed", true: "_duplicate"}[duplicate], func(t *testing.T) {
				storage, ctx, repo := graphProjectionPG(t)
				initial := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
				graphProjectionPublishPG(t, ctx, storage, initial)
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='source advances before terminal failure' WHERE repo_id=$1`, string(repo))
				failure := graphProjectionCorruptPG(t, ctx, storage, repo)
				if duplicate {
					if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
						t.Fatal(err)
					}
				}
				active := graphProjectionClaimPG(t, ctx, storage, repo)
				if phase == "project" {
					active = graphProjectionCapturePG(t, ctx, storage, active, 0)
				}
				if err := storage.FailGraphProjection(ctx, active, "invalid_source", false); err != nil {
					t.Fatal(err)
				}
				before := graphProjectionSnapshotPG(t, ctx, storage, repo)
				ledgerBefore := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
				if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
					t.Fatal(err)
				}
				ledgerAfter := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
				if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) {
					t.Fatal("delayed corruption receipt cleared a terminal source failure")
				}
				if duplicate {
					if ledgerAfter.requests != ledgerBefore.requests+1 || !reflect.DeepEqual(ledgerAfter.rebuilds, ledgerBefore.rebuilds) || ledgerAfter.quarantined != ledgerBefore.quarantined {
						t.Fatal("duplicate receipt restarted terminal work or spent repair budget")
					}
				} else if !reflect.DeepEqual(ledgerAfter, ledgerBefore) {
					t.Fatal("terminal source failure scheduled a new repair")
				}
				if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("terminal worker became claimable: %v", err)
				}
			})
		}
	}

}

func TestPGGraphProjectionRepairRechecksCurrentIdentity(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	failure := graphProjectionCorruptPG(t, ctx, storage, repo)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	for _, ids := range []struct{ peer, binary []domain.DocumentIdentity }{
		{}, {peer: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}}, {binary: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}},
	} {
		bound := outbound.WithDocumentIdentityCompatibility(ctx, ids.peer, ids.binary)
		if err := storage.RequestGraphProjectionRepair(bound, failure); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			t.Fatalf("stale compatibility scheduled repair: %v", err)
		}
	}
	if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || graphProjectionRepairLedgerPG(t, ctx, storage, repo).requests != 0 {
		t.Fatal("incompatible repair changed state")
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.RequestGraphProjectionRepair(compatible, failure); err != nil {
		t.Fatal(err)
	}
	if ledger := graphProjectionRepairLedgerPG(t, ctx, storage, repo); ledger.requests != 1 || ledger.quarantined {
		t.Fatalf("compatible repair failed: %+v", ledger)
	}
}

func TestPGGraphProjectionRepairQuarantineSurvivesStartup(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	var failure domain.GraphProjectionCorruption
	for attempt := 1; attempt <= 4; attempt++ {
		job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
		graphProjectionPublishPG(t, ctx, storage, job)
		failure = graphProjectionCorruptPG(t, ctx, storage, repo)
		if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
			t.Fatal(err)
		}
		ledger := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
		if ledger.requests != int64(attempt) || len(ledger.rebuilds) != min(attempt, 3) || ledger.quarantined != (attempt == 4) {
			t.Fatalf("repair limit failed at %d: %+v", attempt, ledger)
		}
	}
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	ledger := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
	restarted, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || !reflect.DeepEqual(ledger, graphProjectionRepairLedgerPG(t, ctx, storage, repo)) {
		t.Fatal("startup silently cleared repair quarantine")
	}
	for _, enabled := range []bool{false, true} {
		if err := restarted.ConfigureGraphProjection(ctx, repo, enabled); err != nil {
			t.Fatal(err)
		}
		status, err := restarted.ReadGraphProjectionStatus(ctx, repo)
		if err != nil || !status.NeedsAttention || status.Enabled != enabled || status.Complete || status.Ready {
			t.Fatalf("quarantine bypassed on enable=%v: %+v %v", enabled, status, err)
		}
	}
	if _, err := restarted.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("quarantined worker was claimable: %v", err)
	}
	before = graphProjectionSnapshotPG(t, ctx, storage, repo)
	if err := restarted.RequestGraphProjectionRepair(ctx, failure); err != nil {
		t.Fatal(err)
	}
	after := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || after.requests != ledger.requests+1 ||
		!after.quarantined || !reflect.DeepEqual(after.rebuilds, ledger.rebuilds) {
		t.Fatal("duplicate quarantine receipt reset work or spent budget")
	}
}

func TestPGGraphProjectionRepairBudgetExpiresWithoutGrowingLedger(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	for attempt := 0; attempt < 3; attempt++ {
		job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
		graphProjectionPublishPG(t, ctx, storage, job)
		if err := storage.RequestGraphProjectionRepair(ctx, graphProjectionCorruptPG(t, ctx, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_repairs SET rebuilds=ARRAY[clock_timestamp()-interval '61 minutes'],
		window_started_at=clock_timestamp()-interval '61 minutes' WHERE repo_id=$1`, string(repo))
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	if err := storage.RequestGraphProjectionRepair(ctx, graphProjectionCorruptPG(t, ctx, storage, repo)); err != nil {
		t.Fatal(err)
	}
	ledger := graphProjectionRepairLedgerPG(t, ctx, storage, repo)
	if ledger.requests != 4 || len(ledger.rebuilds) != 1 || ledger.quarantined || !ledger.window.Equal(ledger.rebuilds[0]) {
		t.Fatalf("expired repair budget did not reopen bounded window: %+v", ledger)
	}
}

type graphProjectionRepairFaultTx struct {
	pgx.Tx
	needle  string
	failure error
}

func (transaction graphProjectionRepairFaultTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, transaction.needle) {
		return graphProjectionRepairFaultRow{transaction.failure}
	}
	return transaction.Tx.QueryRow(ctx, query, args...)
}

type graphProjectionRepairFaultRow struct{ failure error }

func (row graphProjectionRepairFaultRow) Scan(...any) error { return row.failure }

func TestPGGraphProjectionReadDoesNotMislabelSourceIdentityOrSQLFailures(t *testing.T) {
	for _, mode := range []string{"source", "identity", "sql", "cancel", "old_rules", "reset", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			graphProjectionPublishPG(t, ctx, storage, job)
			graphProjectionCorruptPG(t, ctx, storage, repo)
			var want error
			switch mode {
			case "source":
				catalogExecPG(t, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(repo))
				want = domain.ErrIntegrity
			case "identity":
				if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrDocumentIdentityUpgradeRequired
			case "sql":
				want = &pgconn.PgError{Code: "57014", Message: "statement timeout"}
			case "cancel":
				want = context.Canceled
			case "old_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=1 WHERE repo_id=$1`, string(repo))
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "disabled":
				if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			}
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			var read domain.GraphProjectionRead
			err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				if mode == "sql" {
					transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
					transaction.Tx = graphProjectionRepairFaultTx{Tx: transaction.Tx, needle: "FROM graph_projection_parts", failure: want}
					bound = context.WithValue(bound, repositoryTxKey{}, &transaction)
				}
				if mode == "cancel" {
					var cancel context.CancelFunc
					bound, cancel = context.WithCancel(bound)
					cancel()
				}
				var err error
				read, err = storage.ReadGraphProjection(bound, repo)
				return err
			})
			var failure *domain.GraphProjectionCorruption
			if !errors.Is(err, want) || errors.As(err, &failure) || (err != nil && !reflect.DeepEqual(read, domain.GraphProjectionRead{})) {
				t.Fatalf("non-output failure produced receipt or metadata: %+v %v want=%v", read, err, want)
			}
			if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || graphProjectionRepairLedgerPG(t, ctx, storage, repo).requests != 0 {
				t.Fatal("read failure wrote repair state")
			}
		})
	}
}
