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
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryWorkProbePG struct {
	*store.PostgresStore
	versions     []int
	beforeFinish func()
}

func (probe *memoryWorkProbePG) WriteMemoryCapture(ctx context.Context, bundle domain.MemoryCaptureBundle, expiry time.Time) error {
	probe.versions = append(probe.versions, bundle.Header.Version)
	return probe.PostgresStore.WriteMemoryCapture(ctx, bundle, expiry)
}

func (probe *memoryWorkProbePG) FinishMinimumMemoryReadWork(ctx context.Context, work domain.MemoryReadWork, qualification domain.MemoryReadQualification) (domain.ContentHash, error) {
	if probe.beforeFinish != nil {
		callback := probe.beforeFinish
		probe.beforeFinish = nil
		callback()
	}
	return probe.PostgresStore.FinishMinimumMemoryReadWork(ctx, work, qualification)
}

func memoryWorkConfigurePG(test *testing.T, service *Service, storage *store.PostgresStore, repo domain.ContentHash) {
	test.Helper()
	if err := service.ConfigureMemoryReadPreparations(systemTestContext(), []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := storage.ConfigureMemoryReadWork(systemTestContext(), repo, false); err != nil {
			test.Error(err)
		}
	})
}

func TestPGMemoryReadWorkerCurrentSelectionRestartAndDelta(test *testing.T) {
	service, storage, _, repo, request, initial := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	previousHash := request.Selection.MemoryHash
	request.Selection.MemoryHash = ""
	memoryWorkConfigurePG(test, service, storage, repo)
	probe := &memoryWorkProbePG{PostgresStore: storage}
	service.meta = probe
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("unprepared memory did not return a preparation state", err)
	}
	var anchor *PreparedMemoryRead
	var firstID domain.ContentHash
	for iteration := 0; iteration < 3; iteration++ {
		if iteration > 0 {
			digest := capturedMemoryDigestPG(request.Selection.SnapshotID, "worker updated")
			digest.PreviousMemoryHash = previousHash
			digest.Summary = time.Unix(int64(iteration), 0).String()
			var err error
			previousHash, err = service.PutMemoryDigestCAS(ctx, repo, digest)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
				test.Fatal("old result advertised current after source changed", err)
			}
		}
		queued, err := service.RequestMemoryReadPreparation(ctx, repo, request)
		if err != nil || queued.State != "waiting" {
			test.Fatal("enqueue", queued.State, err)
		}
		if firstID == "" {
			firstID = queued.ID
		} else if firstID != queued.ID {
			test.Fatal("same target generated another work identity")
		}
		before, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if err := service.processMemoryReadPreparations(ctx, 1, &anchor); err != nil {
			test.Fatal("worker", err)
		}
		if anchor == nil || anchor.graphAnchor == nil || !reflect.DeepEqual(*anchor, PreparedMemoryRead{graphAnchor: anchor.graphAnchor}) {
			test.Fatal("worker retained memory output rather than only one graph anchor")
		}
		completed, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
		if err != nil || completed.State != "completed" || completed.QualificationID == "" {
			test.Fatal("completion", completed.State, err)
		}
		after, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
		if err != nil || after != before {
			test.Fatal("read preparation rewrote business source", err)
		}
		pageRequest := request
		for page := 0; ; page++ {
			want, err := service.QueryEffectiveMemory(ctx, repo, pageRequest)
			if err != nil {
				test.Fatal(err)
			}
			got, err := service.QueryCurrentMemory(ctx, repo, pageRequest)
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatal("selected durable page differs from existing query", iteration, page, err)
			}
			if got.NextCursor == "" {
				break
			}
			if page > 30 || len(got.Items) == 0 {
				test.Fatal("pagination did not advance")
			}
			pageRequest.Cursor = got.NextCursor
		}
	}
	if !reflect.DeepEqual(probe.versions, []int{domain.MemoryCaptureVersion, domain.MemoryCaptureDeltaVersion, domain.MemoryCaptureDeltaVersion}) {
		test.Fatal("ordinary updates did not reuse a longer-lived graph anchor", probe.versions)
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewService(reopened, reopened, auth.NewTeamTokenAuth(), gitengine.NewEngine(reopened), reopened)
	want, err := service.QueryCurrentMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	got, err := restarted.QueryCurrentMemory(ctx, repo, request)
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatal("restart lost current pointer", err)
	}
	if initial.input.Header.Version != domain.MemoryCaptureVersion {
		test.Fatal("fixture identity changed")
	}
}

func TestPGMemoryReadWorkerLateObjectChangeNeverPublishesPointer(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	probe := &memoryWorkProbePG{PostgresStore: storage, beforeFinish: func() {
		if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, request.Selection.MemoryHash); err != nil {
			test.Fatal(err)
		}
	}}
	service.meta = probe
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late physical-object change was published", err)
	}
	status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || status.QualificationID != "" || status.State != "retrying" {
		test.Fatal("failure lost retry state or advertised pointer", status.State, err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("failed completion exposed result", err)
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("fresh source retry", err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadWorkerCurrentAuthorityAndScope(test *testing.T) {
	service, storage, _, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	if _, err := service.RequestMemoryReadPreparation(context.Background(), repo, request); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("request manufactured system authority", err)
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryCurrentMemory(context.Background(), repo, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("missing authority read cached result", err)
	}
	denied := inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
		return nil, domain.ErrForbidden
	})
	if page, err := service.QueryCurrentMemory(denied, repo, request); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
		test.Fatal("cached result bypassed snapshot authority", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := service.ReadMemoryPreparationStatus(bound, repo, request); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("status borrowed stale snapshot", err)
		}
		if _, err := service.QueryCurrentMemory(bound, repo, request); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("current selection borrowed stale snapshot", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	readOnly := outbound.WithDocReadOnly(ctx)
	if _, err := service.RequestMemoryReadPreparation(readOnly, repo, request); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("read-only path enqueued work", err)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, repo, false); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("disabled repository still served selected output", err)
	}
}
