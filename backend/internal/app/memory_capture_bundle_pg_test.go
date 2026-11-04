//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func roundTripMemoryCapturePG(test *testing.T, storage *store.PostgresStore, dsn string, captured *CapturedEffectiveMemory) *CapturedEffectiveMemory {
	test.Helper()
	ctx := systemTestContext()
	bundle, err := captured.Export(ctx)
	if err != nil {
		test.Fatal("export", err)
	}
	if err := storage.WriteMemoryCapture(ctx, bundle, time.Now().Add(time.Hour)); err != nil {
		test.Fatal("persist", err)
	}
	reopened, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	loaded, err := reopened.ReadMemoryCapture(ctx, captured.Generation().Graph.RepoID, bundle.ID)
	reopened.Close()
	if err != nil || loaded.ID != bundle.ID || loaded.Header != bundle.Header || domain.ValidateMemoryCapture(ctx, loaded) != nil {
		test.Fatal("restarted store changed captured inputs", err)
	}
	restored, err := RestoreCapturedEffectiveMemory(ctx, loaded, memoryBundleFactory{})
	if err != nil || !reflect.DeepEqual(restored.Dependencies(), captured.Dependencies()) {
		test.Fatal("restore changed dependency closure", err)
	}
	roundTripMemoryReadPG(test, storage, dsn, restored)
	return restored
}

func TestPGMemoryCaptureBundleSurvivesOriginalMutationAndRestart(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	snapshot := collaborationSnapshot(test, storage, repo, "durable captured memory")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "before mutation"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(3)}, Limit: 1}
	captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal(err)
	}
	pages := capturedMemoryPagesPG(test, service, repo, captured, request)
	bundle, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WriteMemoryCapture(ctx, bundle, time.Now().Add(time.Hour)); err != nil {
		test.Fatal(err)
	}
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(memory)); err != nil {
		test.Fatal(err)
	}
	connection.Close()
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("current query inherited historical capture authority", err)
	}
	storage.Close()
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	loaded, err := reopened.ReadMemoryCapture(ctx, repo, bundle.ID)
	reopened.Close()
	if err != nil {
		test.Fatal(err)
	}
	restored, err := RestoreCapturedEffectiveMemory(ctx, loaded, memoryBundleFactory{})
	if err != nil {
		test.Fatal(err)
	}
	assertFrozenMemoryPagesPG(test, restored, pages)
}
