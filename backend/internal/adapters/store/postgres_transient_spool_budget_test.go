//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func pgTransientSpoolBudgetCapability(test *testing.T, storage any) outbound.TransientSpoolBudgetStore {
	test.Helper()
	capability, supported := storage.(outbound.TransientSpoolBudgetStore)
	if !supported {
		test.Fatalf("store %T does not expose transient spool accounting", storage)
	}
	return capability
}

func pgTransientSpoolBudgetUsage(test *testing.T, capability outbound.TransientSpoolBudgetStore, limit, used int64, open int) outbound.TransientSpoolBudgetUsage {
	test.Helper()
	usage := capability.TransientSpoolBudgetUsage()
	if usage.LimitBytes != limit || usage.UsedBytes != used || usage.OpenFiles != open || usage.PeakBytes < used ||
		limit > 0 && usage.PeakBytes > limit {
		test.Fatal("transient spool accounting disagrees with live reservations", usage, limit, used, open)
	}
	return usage
}

func pgTransientSpoolBudgetStore(test *testing.T) (*PostgresStore, context.Context) {
	test.Helper()
	fixture := processAdmissionPG(test)
	storage := fixture.dataStore(test, outbound.ProcessAPI, nil)
	if _, err := storage.ApplyMigrations(fixture.ctx, "../../../../schemas/db/migrations"); err != nil {
		test.Fatal("initialize only the owned local spool fixture", err)
	}
	return storage, fixture.ctx
}

func TestPGTransientSpoolBudgetPublicProxiesSharePositiveConfigurationAndDrain(test *testing.T) {
	directory := rangeSpoolTempDir(test)
	filesystem := pgTransientSpoolBudgetCapability(test, NewFSStore(test.TempDir()))
	postgres := pgTransientSpoolBudgetCapability(test, &PostgresStore{})
	pgTransientSpoolBudgetUsage(test, filesystem, 0, 0, 0)
	for _, limit := range []int64{0, -1} {
		restore, err := postgres.ConfigureTransientSpoolBudget(limit)
		if restore != nil {
			_ = restore()
			test.Fatal("invalid explicit configuration returned a live restoration callback", limit)
		}
		if err == nil {
			test.Fatal("PostgreSQL transient spool configuration accepted a nonpositive cap", limit)
		}
	}
	restore, err := filesystem.ConfigureTransientSpoolBudget(8)
	if err != nil || restore == nil {
		test.Fatal("configure through the filesystem capability", err)
	}
	test.Cleanup(func() {
		if err := restore(); err != nil {
			test.Error("restore shared spool configuration", err)
		}
	})
	if overlap, err := postgres.ConfigureTransientSpoolBudget(16); err == nil || overlap != nil {
		if overlap != nil {
			_ = overlap()
		}
		test.Fatal("PostgreSQL installed a second process-local spool budget", err)
	}
	legacy, err := newLegacyDocSpool()
	if err != nil {
		test.Fatal(err)
	}
	defer legacy.Close()
	ranges, err := newDocRangeSpool()
	if err != nil {
		test.Fatal(err)
	}
	defer ranges.Close()
	if err := legacy.Append(context.Background(), []byte("legacy")); err != nil {
		test.Fatal(err)
	}
	if err := ranges.Append(context.Background(), []byte("pg")); err != nil {
		test.Fatal(err)
	}
	want := pgTransientSpoolBudgetUsage(test, filesystem, 8, 8, 2)
	if got := postgres.TransientSpoolBudgetUsage(); got != want {
		test.Fatal("filesystem and PostgreSQL proxies do not share actual spool reservations", got, want)
	}
	if err := ranges.Append(context.Background(), []byte("!")); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("buffered range bytes escaped the cumulative process cap", err)
	}
	if err := ranges.Flush(context.Background()); err != nil {
		test.Fatal(err)
	}
	if information, err := ranges.file.Stat(); err != nil || information.Size() != 2 || len(ranges.offsets) != 2 {
		test.Fatal("rejected range append changed the backing file or page directory", information, err)
	}
	if err := restore(); err == nil {
		test.Fatal("restoration succeeded while allocations remained open")
	}
	pgTransientSpoolBudgetUsage(test, postgres, 8, 8, 2)
	if blocked, err := newLegacyDocSpool(); err == nil || blocked != nil {
		if blocked != nil {
			_ = blocked.Close()
		}
		test.Fatal("draining configuration admitted a new allocation", err)
	}
	if err := legacy.Close(); err != nil {
		test.Fatal(err)
	}
	pgTransientSpoolBudgetUsage(test, filesystem, 8, 2, 1)
	if err := ranges.Close(); err != nil {
		test.Fatal(err)
	}
	pgTransientSpoolBudgetUsage(test, postgres, 0, 0, 0)
	if err := restore(); err != nil {
		test.Fatal("drained restoration was not idempotent", err)
	}
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("shared budget cleanup retained backing files", files, err)
	}
	reconfigured, err := postgres.ConfigureTransientSpoolBudget(9)
	if err != nil || reconfigured == nil {
		test.Fatal("PostgreSQL could not configure after filesystem-owned drainage", err)
	}
	defer reconfigured()
	pgTransientSpoolBudgetUsage(test, filesystem, 9, 0, 0)
	if err := reconfigured(); err != nil {
		test.Fatal(err)
	}
}

type pgTransientSpoolBudgetContext struct {
	context.Context
	directory  string
	file       *os.File
	observeErr error
}

func (observed *pgTransientSpoolBudgetContext) Err() error {
	if observed.file == nil && observed.observeErr == nil {
		var files []os.DirEntry
		files, observed.observeErr = os.ReadDir(observed.directory)
		for _, entry := range files {
			if strings.HasPrefix(entry.Name(), "cxthub-search-events-") {
				observed.file, observed.observeErr = os.Open(filepath.Join(observed.directory, entry.Name()))
				break
			}
		}
	}
	return observed.Context.Err()
}

func TestPGTransientSpoolBudgetSearchReservesBeforeWritingAndDoesNotPublish(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		limit int64
		size  int64
	}{
		{name: "first-copy", limit: (32 << 10) - 1},
		{name: "later-copy", limit: (32 << 10) + 13, size: 32 << 10},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			capability := pgTransientSpoolBudgetCapability(test, &PostgresStore{})
			restore, err := capability.ConfigureTransientSpoolBudget(scenario.limit)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if err := restore(); err != nil {
					test.Error(err)
				}
			})
			doc := readBlockDoc(test, test.Name(), strings.Repeat("SearchNeedle ", 8192), 0)
			plan, err := doc.PlanReadIndex()
			if err != nil {
				test.Fatal(err)
			}
			observed := &pgTransientSpoolBudgetContext{Context: context.Background(), directory: directory}
			defer func() {
				if observed.file != nil {
					_ = observed.file.Close()
				}
			}()
			transaction := &readBlockBatchTxPG{execute: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
				test.Fatal("overbudget search spool attempted to publish text")
				return pgconn.CommandTag{}, nil
			}}
			if err := putReadBlockEventsPG(observed, transaction, plan.Blocks()); !errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("search copy reclassified or ignored reservation failure", err)
			}
			if observed.observeErr != nil || observed.file == nil {
				test.Fatal("did not observe the actual production search spool", observed.observeErr)
			}
			if information, err := observed.file.Stat(); err != nil || information.Size() != scenario.size {
				test.Fatal("overbudget copy wrote bytes before acquiring its reservation", information, scenario.size, err)
			}
			usage := pgTransientSpoolBudgetUsage(test, capability, scenario.limit, 0, 0)
			if usage.PeakBytes != scenario.size || len(transaction.locations) != 0 {
				test.Fatal("failed search copy retained reservations or published partial locations", usage, transaction.locations)
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
				test.Fatal("failed search copy retained its temporary path", files, err)
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := putReadBlockEventsPG(canceled, transaction, plan.Blocks()); !errors.Is(err, context.Canceled) {
				test.Fatal("spool quota displaced an already-canceled search", err)
			}
		})
	}
}

func TestPGTransientSpoolBudgetPublicationRollbackAndRecovery(test *testing.T) {
	for _, mode := range []string{"direct", "staged"} {
		test.Run(mode, func(test *testing.T) {
			storage, ctx := pgTransientSpoolBudgetStore(test)
			directory := rangeSpoolTempDir(test)
			capability := pgTransientSpoolBudgetCapability(test, storage)
			repo := domain.HashContent([]byte(test.Name()))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				test.Fatal(err)
			}
			doc := readBlockDoc(test, string(repo), strings.Repeat("SearchNeedle MiXeD 50%_\\ \ud55c🙂 ", 4096), 1)
			index, err := doc.ReadIndex()
			if err != nil {
				test.Fatal(err)
			}
			var limit int64
			for _, event := range index.Events {
				limit += int64(len(event.Text))
			}
			plan, err := doc.PlanReadIndex()
			if err != nil || limit <= 32<<10 {
				test.Fatal("fixture must require multiple bounded search copies", limit, err)
			}
			publish := func() error {
				_, err := storage.PutVerifiedDoc(ctx, repo, doc)
				return err
			}
			var job domain.DocFinalizationJob
			var publication pgDocPublication
			if mode == "staged" {
				job, publication = stageJobForDocPG(test, storage, ctx, repo, doc)
				publish = func() error { return publication.Complete(ctx, job, time.Now().UTC()) }
			}
			restore, err := capability.ConfigureTransientSpoolBudget(limit)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if err := restore(); err != nil {
					test.Error(err)
				}
			})
			holder, err := newDocRangeSpool()
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Close()
			if err := holder.Append(ctx, []byte("!")); err != nil {
				test.Fatal(err)
			}
			if err := publish(); !errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("actual PostgreSQL publication bypassed the shared transient budget", err)
			}
			usage := pgTransientSpoolBudgetUsage(test, capability, limit, 1, 1)
			if usage.PeakBytes <= 1 {
				test.Fatal("fixture never exercised a partially written search spool", usage)
			}
			for _, block := range plan.Blocks() {
				stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=$1`, block.Hash)
				stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM doc_read_block_events_v3 WHERE block_hash=$1`, block.Hash)
			}
			stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=ANY($1::text[])`, plan.EventHashes())
			stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM blobs WHERE hash=$1`, doc.Hash())
			stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, doc.Hash())
			stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM doc_read_index_current WHERE hash=$1`, doc.Hash())
			stageCountPG(test, storage, ctx, 0, `SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=$1`, doc.Hash())
			if mode == "staged" {
				stagePinsPG(test, storage, ctx, job, publication, false)
				stageInvisiblePG(test, storage, ctx, job)
			} else {
				if _, err := storage.GetDoc(ctx, repo, doc.Hash()); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("failed direct publication exposed a document", err)
				}
				if hits, err := storage.SearchDocEvents(ctx, repo, doc.Hash(), "searchneedle", -1, 10); !errors.Is(err, domain.ErrNotFound) || len(hits) != 0 {
					test.Fatal("failed direct publication exposed partial search results", hits, err)
				}
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 1 || !strings.HasPrefix(files[0].Name(), "cxthub-doc-range-") {
				test.Fatal("search rollback leaked a spool or removed the unrelated allocation", files, err)
			}
			if err := holder.Close(); err != nil {
				test.Fatal(err)
			}
			pgTransientSpoolBudgetUsage(test, capability, limit, 0, 0)
			if err := publish(); err != nil {
				test.Fatal("publication did not recover under the unchanged cap after cleanup", err)
			}
			usage = pgTransientSpoolBudgetUsage(test, capability, limit, 0, 0)
			if usage.PeakBytes != limit {
				test.Fatal("successful recovery did not account for the exact search-text extent", usage, limit)
			}
			if mode == "staged" {
				stageCompletedPG(test, storage, ctx, job, publication)
			}
			got, err := storage.ReadVerifiedDoc(ctx, repo, doc.Hash())
			if err != nil || got.DocumentRef() != doc.DocumentRef() || !bytes.Equal(got.Bytes(), doc.Bytes()) {
				test.Fatal("recovery changed document bytes, hash or identity", err)
			}
			wantMetadata := index
			wantMetadata.Events = append([]domain.DocEventIndex(nil), index.Events...)
			for position := range wantMetadata.Events {
				wantMetadata.Events[position].Text = ""
			}
			if got, err := storage.DocReadIndex(ctx, repo, doc.Hash()); err != nil || !reflect.DeepEqual(got, wantMetadata) {
				test.Fatal("recovery changed exact read coordinates, hashes or roles", got, err)
			}
			hits, err := storage.SearchDocEvents(ctx, repo, doc.Hash(), "searchneedle", -1, 10)
			wantHit := index.Events[1]
			wantHit.Offset, wantHit.Length = 0, 0
			if err != nil || !reflect.DeepEqual(hits, []domain.DocEventIndex{wantHit}) {
				test.Fatal("recovery published incomplete or changed search text", hits, err)
			}
			foreign := domain.HashContent([]byte("foreign " + string(repo)))
			if hits, err := storage.SearchDocEvents(ctx, foreign, doc.Hash(), "searchneedle", -1, 10); !errors.Is(err, domain.ErrNotFound) || len(hits) != 0 {
				test.Fatal("budget recovery granted foreign search access", hits, err)
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
				test.Fatal("successful recovery retained transient files", files, err)
			}
		})
	}
}
