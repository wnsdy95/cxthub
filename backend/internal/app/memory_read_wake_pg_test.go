//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func assertCurrentMemoryParityPG(test *testing.T, service *Service, repo domain.ContentHash, request domain.EffectiveMemoryRequest) {
	test.Helper()
	for page := 0; ; page++ {
		want, err := service.QueryEffectiveMemory(systemTestContext(), repo, request)
		if err != nil {
			test.Fatal(err)
		}
		got, err := service.QueryCurrentMemory(systemTestContext(), repo, request)
		if err != nil || !reflect.DeepEqual(got, want) {
			test.Fatal("automatic result differs from existing query", page, err)
		}
		if got.NextCursor == "" {
			return
		}
		if page > 30 || len(got.Items) == 0 {
			test.Fatal("automatic result pagination did not progress")
		}
		request.Cursor = got.NextCursor
	}
}

func TestPGMemoryReadWakeAutomaticallyPreparesChangedSourceAfterRestart(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	previous := request.Selection.MemoryHash
	request.Selection.MemoryHash = ""
	memoryWorkConfigurePG(test, service, storage, repo)
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	prior, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	digest := capturedMemoryDigestPG(request.Selection.SnapshotID, "automatically prepared after restart")
	digest.PreviousMemoryHash = previous
	if _, err := service.PutMemoryDigestCAS(ctx, repo, digest); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("changed source was falsely current", err)
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewService(reopened, reopened, auth.NewTeamTokenAuth(), gitengine.NewEngine(reopened), reopened)
	if err := restarted.ConfigureMemoryReadPreparations(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := restarted.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("durable source notification did not resume", err)
	}
	completed, err := restarted.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || completed.State != "completed" || completed.ID != prior.ID || completed.Version <= prior.Version ||
		completed.QualificationID == prior.QualificationID || completed.Source == prior.Source {
		test.Fatal("source notification did not replace completed generation", completed.State, err)
	}
	assertCurrentMemoryParityPG(test, restarted, repo, request)
}

func TestPGMemoryReadWakePhysicalRepairWithoutLogicalSourceChange(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	source, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	var original []byte
	if err := connection.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, request.Selection.MemoryHash).Scan(&original); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=convert_to('invalid memory bytes','UTF8') WHERE hash=$1`, request.Selection.MemoryHash); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("physical corruption was not rejected", err)
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || status.State != "rejected" || status.Reason != "invalid_source" {
		test.Fatal("corrupt source did not surface a terminal cause", status.State, status.Reason, err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNeedsAttention) {
		test.Fatal("old pointer hid corrupt input", err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, request.Selection.MemoryHash, original); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("repair required an extra client request", err)
	}
	if current, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo); err != nil || current != source {
		test.Fatal("fixture changed logical generation", err)
	}
	assertCurrentMemoryParityPG(test, service, repo, request)
}

type expiringMemoryWakeProbePG struct {
	*store.PostgresStore
	first bool
}

func (probe *expiringMemoryWakeProbePG) WriteMemoryCapture(ctx context.Context, capture domain.MemoryCaptureBundle, expiry time.Time) error {
	if !probe.first {
		probe.first = true
		expiry = time.Now().Add(time.Second)
	}
	return probe.PostgresStore.WriteMemoryCapture(ctx, capture, expiry)
}

func TestPGMemoryReadWakeAutomaticallyRecoversQuietExpiry(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	service.meta = &expiringMemoryWakeProbePG{PostgresStore: storage}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	initial, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	var remaining float64
	if err := connection.QueryRow(ctx, `SELECT GREATEST(0,extract(epoch FROM (next_attempt-clock_timestamp())))
 FROM memory_read_wakes WHERE repo_id=$1`, repo).Scan(&remaining); err != nil {
		test.Fatal("completion did not schedule expiry", err)
	}
	if remaining > 2 {
		test.Fatal("expiry wake was not bounded", remaining)
	}
	time.Sleep(time.Duration(remaining*float64(time.Second)) + 10*time.Millisecond)
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("expired output accepted", err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("expiry recovery required a new request or source change", err)
	}
	completed, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || completed.State != "completed" || completed.Version <= initial.Version || completed.Source != initial.Source {
		test.Fatal("quiet source did not recover", completed.State, err)
	}
	assertCurrentMemoryParityPG(test, service, repo, request)
}
