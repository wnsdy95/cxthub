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

func roundTripMemoryReadPG(test *testing.T, storage *store.PostgresStore, dsn string, captured *CapturedEffectiveMemory) domain.MemoryReadBundle {
	test.Helper()
	ctx := systemTestContext()
	input, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	result, err := BuildMemoryRead(ctx, input, memoryBundleFactory{})
	if err != nil {
		test.Fatal("prepare complete memory result", err)
	}
	if err := storage.WriteMemoryCapture(ctx, input, time.Now().Add(time.Hour)); err != nil {
		test.Fatal(err)
	}
	if err := storage.WriteMemoryRead(ctx, result); err != nil {
		test.Fatal("persist complete memory result", err)
	}
	reopened, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	for _, limits := range [][]int{{1}, {50}, {1, 3, 0, 2}} {
		request := domain.EffectiveMemoryRequest{Selection: captured.selection, Content: captured.content}
		for ordinal := 0; ; ordinal++ {
			request.Limit = limits[ordinal%len(limits)]
			want, err := captured.Project(ctx, request)
			if err != nil {
				test.Fatal(err)
			}
			var actual domain.EffectiveMemoryPage
			if err := reopened.WithinReadSnapshot(ctx, func(bound context.Context) error {
				var readErr error
				actual, readErr = ReadMemoryReadPage(bound, reopened, captured.reader.repo, result.ID, request)
				return readErr
			}); err != nil || !reflect.DeepEqual(actual, want) {
				test.Fatalf("persisted memory differs from captured selection: %v\ngot %+v\nwant %+v", err, actual, want)
			}
			if actual.NextCursor == "" {
				break
			}
			if ordinal > 100 || len(actual.Items) == 0 {
				test.Fatal("persisted memory cursor did not advance")
			}
			request.Cursor = actual.NextCursor
		}
	}
	return result
}

func TestPGMemoryReadApplicationRestartIsNotCurrentAuthority(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 45*time.Second)
	defer cancel()
	snapshot := collaborationSnapshot(test, storage, repo, "persisted memory result")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, capturedMemoryDigestPG(snapshot, "version one"))
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(3)}, Limit: 1}
	captured, err := service.CaptureEffectiveMemorySource(ctx, repo, request, nil)
	if err != nil {
		test.Fatal(err)
	}
	pages := capturedMemoryPagesPG(test, service, repo, captured, request)
	result := roundTripMemoryReadPG(test, storage, collaborationDSN(test), captured)
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, repo, memory); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("persisted result replaced current source ownership", err)
	}
	storage.Close()
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	for _, expected := range pages {
		actual, err := ReadMemoryReadPage(ctx, reopened, repo, result.ID, expected.request)
		if err != nil || !reflect.DeepEqual(actual, expected.page) {
			test.Fatal("historical internal result changed after current ownership removal", err)
		}
	}
}
