//go:build postgres

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

type displayMaintenanceProcessReport struct {
	PID            int
	Steps          []domain.DocDisplayMaintenanceStep
	Calls          int
	System         bool
	ReadOnlyFenced bool
	Canceled       bool
	Idle           bool
	Joined         bool
	Closed         bool
}

type displayMaintenanceProcessObserver struct {
	outbound.DocDisplayMaintenanceStore
	report   displayMaintenanceProcessReport
	returned chan struct{}
}

func (observer *displayMaintenanceProcessObserver) StepDocDisplayMaintenance(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
	_, system := inbound.RepositoryActor(ctx)
	observer.report.System = observer.report.System && system
	step, err := observer.DocDisplayMaintenanceStore.StepDocDisplayMaintenance(ctx)
	observer.report.Calls++
	if err == nil {
		observer.report.Steps = append(observer.report.Steps, step)
	}
	if errors.Is(err, domain.ErrNotFound) {
		observer.report.Idle = true
	}
	if observer.returned != nil {
		close(observer.returned)
		observer.returned = nil
	}
	return step, err
}

func assertDisplayMaintenanceProcessBounds(test *testing.T, step domain.DocDisplayMaintenanceStep) {
	test.Helper()
	if domain.ValidateContentHash(step.Source) != nil || step.Phase == "" ||
		step.Indexed < 0 || step.Indexed > 8 ||
		step.Marked < 0 || step.Marked > domain.DocDisplayMaintenanceBatch ||
		step.Deleted < 0 || step.Deleted > domain.DocDisplayMaintenanceBatch || step.DeletedBytes < 0 ||
		step.CatalogJobs < 0 || step.CatalogJobs > domain.DocDisplayCatalogCensusBatch ||
		step.CatalogDeleted < 0 || step.CatalogDeleted > domain.DocDisplayMaintenanceBatch/4 {
		test.Fatal("collector exceeded the original store step bounds", step)
	}
}

func TestDisplayMaintenanceProcessPGChild(test *testing.T) {
	mode := os.Getenv("CXT_DISPLAY_MAINTENANCE_PROCESS_CHILD")
	if mode == "" {
		test.Skip("only entered by the collector subprocess test")
	}
	if mode != "checkpoint" && mode != "resume" {
		test.Fatal("unknown collector child mode")
	}
	dsn := os.Getenv("CXT_TEST_DSN")
	if strings.TrimSpace(dsn) == "" {
		test.Fatal("collector child requires its parent's isolated CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	infrastructure, err := serverruntime.OpenInfrastructure(ctx, serverruntime.InfrastructureConfig{
		DSN: dsn, RequirePostgres: true,
		MigrationsDirectory: os.Getenv("CXT_DISPLAY_MAINTENANCE_PROCESS_MIGRATIONS"),
	})
	if err != nil {
		test.Fatal("child infrastructure", err)
	}
	defer func() {
		if err := infrastructure.Close(); err != nil {
			test.Error("child infrastructure close", err)
		}
	}()
	storage, supported := infrastructure.Store.(outbound.DocDisplayMaintenanceStore)
	if !supported {
		test.Fatal("production infrastructure lacks the maintenance store")
	}
	observer := &displayMaintenanceProcessObserver{
		DocDisplayMaintenanceStore: storage,
		report:                     displayMaintenanceProcessReport{PID: os.Getpid(), System: true},
	}
	worker, err := app.NewDisplayMaintenanceWorker(observer)
	if err != nil {
		test.Fatal("child narrow worker", err)
	}
	if count, err := worker.Process(outbound.WithDocReadOnly(ctx), 1); count != 0 || !errors.Is(err, domain.ErrForbidden) || observer.report.Calls != 0 {
		test.Fatal("read-only worker crossed the mutation fence", count, err, observer.report.Calls)
	}
	observer.report.ReadOnlyFenced = true
	if count, err := worker.Process(ctx, 0); count != 0 || err != nil || observer.report.Calls != 0 {
		test.Fatal("zero limit called the store", count, err, observer.report.Calls)
	}
	canceled, stopCanceled := context.WithCancel(ctx)
	stopCanceled()
	if count, err := worker.Process(canceled, 1); count != 0 || !errors.Is(err, context.Canceled) || observer.report.Calls != 0 {
		test.Fatal("worker lost the original cancellation", count, err, observer.report.Calls)
	}
	finished := false
	for attempt := 0; attempt < 256; attempt++ {
		previous := len(observer.report.Steps)
		count, err := worker.Process(ctx, 1)
		if err != nil || count < 0 || count > 1 || len(observer.report.Steps)-previous != count {
			test.Fatal("collector process step", attempt, count, err)
		}
		if count == 0 {
			if mode == "checkpoint" {
				test.Fatal("collector became idle before useful collection")
			}
			finished = true
			break
		}
		step := observer.report.Steps[previous]
		assertDisplayMaintenanceProcessBounds(test, step)
		if mode == "checkpoint" && step.Deleted > 0 {
			finished = true
			break
		}
	}
	if !finished {
		test.Fatal("small collector fixture failed to reach its process boundary")
	}
	if mode == "resume" {
		running, stopRunning := context.WithCancel(ctx)
		done := make(chan struct{})
		returned := make(chan struct{})
		observer.returned = returned
		go func() {
			worker.Run(running)
			close(done)
		}()
		defer func() {
			stopRunning()
			<-done
		}()
		select {
		case <-returned:
		case <-ctx.Done():
			test.Fatal("Run never returned from an actual store step", ctx.Err())
		}
		stopRunning()
		select {
		case <-done:
			observer.report.Canceled = running.Err() == context.Canceled
			observer.report.Joined = true
		case <-time.After(3 * time.Second):
			test.Fatal("Run failed to join after cancellation")
		}
	}
	for _, step := range observer.report.Steps {
		assertDisplayMaintenanceProcessBounds(test, step)
	}
	if err := infrastructure.Close(); err != nil {
		test.Fatal("close only after all worker calls joined", err)
	}
	observer.report.Closed = true
	raw, err := json.Marshal(observer.report)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CXT_DISPLAY_MAINTENANCE_PROCESS_REPORT"), raw, 0600); err != nil {
		test.Fatal(err)
	}
}

func runDisplayMaintenanceProcessPG(test *testing.T, fixture displayMaintenanceProcessFixture, mode string) displayMaintenanceProcessReport {
	test.Helper()
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	reportPath := filepath.Join(test.TempDir(), "collector.json")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestDisplayMaintenanceProcessPGChild$", "-test.count=1", "-test.timeout=40s")
	command.Dir = test.TempDir()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ", "GOMAXPROCS", "SystemRoot", "WINDIR":
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env,
		"HOME="+command.Dir,
		"CXT_TEST_DSN="+fixture.dsn,
		"CXT_DISPLAY_MAINTENANCE_PROCESS_CHILD="+mode,
		"CXT_DISPLAY_MAINTENANCE_PROCESS_MIGRATIONS="+fixture.migrations,
		"CXT_DISPLAY_MAINTENANCE_PROCESS_REPORT="+reportPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("collector child %s failed: %v (deadline: %v)\n%s", mode, err, ctx.Err(), output)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		test.Fatal("collector report", err)
	}
	var report displayMaintenanceProcessReport
	if err := json.Unmarshal(raw, &report); err != nil {
		test.Fatal("collector report", err)
	}
	if report.PID <= 0 || report.PID == os.Getpid() || !report.System || !report.ReadOnlyFenced || !report.Closed {
		test.Fatal("collector did not use a separate system-scoped owned infrastructure", report)
	}
	for _, step := range report.Steps {
		assertDisplayMaintenanceProcessBounds(test, step)
		if step.Source != fixture.source {
			test.Fatal("collector escaped the single owned display source", step.Source, fixture.source)
		}
	}
	return report
}

func TestDisplayMaintenanceProcessPGCollectorRestartCancellationAndPublishedReads(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := isolatedDisplayMaintenanceProcessDSN(test, ctx)
	fixture := seedDisplayMaintenanceProcessPG(test, ctx, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	before := readDisplayMaintenanceProcessPG(test, ctx, fixture)
	objectsBefore, bytesBefore := displayMaintenanceProcessObjectsPG(test, ctx, pool, fixture.source)
	if remaining := displayMaintenanceProcessOrphansPG(test, ctx, pool, fixture); remaining != domain.DocDisplayMaintenanceBatch+1 {
		test.Fatal("fixture did not exceed exactly one original sweep batch", remaining)
	}
	first := runDisplayMaintenanceProcessPG(test, fixture, "checkpoint")
	if len(first.Steps) == 0 || first.Steps[len(first.Steps)-1].Phase != "sweep" ||
		first.Steps[len(first.Steps)-1].Deleted <= 0 || first.Steps[len(first.Steps)-1].Deleted > domain.DocDisplayMaintenanceBatch || first.Idle {
		test.Fatal("first process did not stop after a useful bounded sweep", first)
	}
	var phase string
	if err := pool.QueryRow(ctx, `SELECT phase FROM doc_display_maintenance_v1 WHERE source_hash=$1`, fixture.source).Scan(&phase); err != nil || phase != "sweep" {
		test.Fatal("first process did not leave a durable resumable sweep", phase, err)
	}
	if remaining := displayMaintenanceProcessOrphansPG(test, ctx, pool, fixture); remaining <= 0 {
		test.Fatal("first process did not leave collection for its replacement", remaining)
	}
	if midway := readDisplayMaintenanceProcessPG(test, ctx, fixture); !reflect.DeepEqual(before, midway) {
		test.Fatal("first collector process changed published text or original-byte evidence")
	}
	second := runDisplayMaintenanceProcessPG(test, fixture, "resume")
	if second.PID == first.PID || len(second.Steps) == 0 || !second.Idle || !second.Canceled || !second.Joined {
		test.Fatal("replacement process did not resume, reach idle, and join before close", second)
	}
	deleted, catalogDeleted := 0, 0
	var deletedBytes int64
	for _, report := range []displayMaintenanceProcessReport{first, second} {
		for _, step := range report.Steps {
			deleted += step.Deleted
			deletedBytes += step.DeletedBytes
			catalogDeleted += step.CatalogDeleted
		}
	}
	objectsAfter, bytesAfter := displayMaintenanceProcessObjectsPG(test, ctx, pool, fixture.source)
	if deleted <= domain.DocDisplayMaintenanceBatch || deletedBytes <= 0 || catalogDeleted == 0 ||
		objectsBefore-objectsAfter != deleted || bytesBefore-bytesAfter != deletedBytes {
		test.Fatal("actual collector reports disagree with durable object reclamation", deleted, deletedBytes, catalogDeleted, objectsBefore, objectsAfter, bytesBefore, bytesAfter)
	}
	if remaining := displayMaintenanceProcessOrphansPG(test, ctx, pool, fixture); remaining != 0 {
		test.Fatal("replacement process retained unreferenced original-format objects", remaining)
	}
	assertDisplayMaintenanceProcessCatalogsPG(test, ctx, pool, false)
	var retired bool
	if err := pool.QueryRow(ctx, `SELECT lifecycle.state='retired' AND lifecycle.retire_phase='empty'
 AND lifecycle.retire_scope='' AND lifecycle.retire_generation=admission.generation AND maintenance.phase='idle'
 FROM doc_display_catalog_lifecycle_v1 lifecycle JOIN doc_display_sources_v1 source USING(source_hash)
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash
 AND admission.profile=source.profile AND admission.source_hash=source.source_hash
 JOIN doc_display_maintenance_v1 maintenance ON maintenance.source_hash=lifecycle.source_hash
 WHERE lifecycle.source_hash=$1`, fixture.source).Scan(&retired); err != nil || !retired {
		test.Fatal("collector lost the original retired incarnation or left pending work", retired, err)
	}
	if after := readDisplayMaintenanceProcessPG(test, ctx, fixture); !reflect.DeepEqual(before, after) {
		test.Fatal("collector restart changed published generation, text, source spans, or archive bytes")
	}
	test.Logf("sequential_collector_children=2 original_sweep_batch=%d deleted_objects=%d deleted_payload_bytes=%d retired_private_rows=%d cancel_join_before_close=true", domain.DocDisplayMaintenanceBatch, deleted, deletedBytes, catalogDeleted)
	test.Log("Minimum nonoverlapping subprocess composition proof only; no simultaneous deployed API/MCP/worker or full RF15 claim.")
}
