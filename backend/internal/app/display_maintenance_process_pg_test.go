//go:build postgres

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
