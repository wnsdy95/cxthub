//go:build postgres

package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGPreparedMemoryPendingReplayAndPinnedPagination(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	request.Limit = 1
	memoryWorkConfigurePG(test, service, storage, repo)
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	expected, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	page, err := service.QueryPreparedMemory(ctx, repo, request)
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
		test.Fatal("first request lost its fixed-floor retry", page, err)
	}
	var workCount int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, repo).Scan(&workCount); err != nil || workCount != 0 {
		test.Fatal("read request enqueued work", workCount, err)
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	service.meta = &memoryWorkProbePG{PostgresStore: storage, beforeFinish: func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' after capture' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	request.Cursor = pending.Cursor
	first, err := service.QueryPreparedMemory(ctx, repo, request)
	if err != nil || first.Status.Freshness != "catching_up" || first.Cursor == "" || first.Page.NextCursor == "" {
		test.Fatal("retry kept chasing latest writes", first.Status, err)
	}
	actual := first.Page
	actual.NextCursor, expected.NextCursor = "", ""
	if !reflect.DeepEqual(actual, expected) {
		test.Fatal("prepared first page differs from legacy at captured source")
	}
	request.Cursor = ""
	if _, err := service.QueryPreparedMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("new request claimed old result as current", err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	request.Cursor = first.Cursor
	replay, err := service.QueryPreparedMemory(ctx, repo, request)
	if err != nil || replay.Status.Generation != first.Status.Generation || replay.Status.Minimum != first.Status.Minimum || !reflect.DeepEqual(replay.Page, first.Page) {
		test.Fatal("fragment replay followed newer work pointer", err)
	}
	request.Cursor = first.Page.NextCursor
	next, err := service.QueryPreparedMemory(ctx, repo, request)
	if err != nil || next.Status.Generation != first.Status.Generation || next.Page.Items[0].ID == first.Page.Items[0].ID {
		test.Fatal("next page did not continue original generation", err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, request.Selection.MemoryHash); err != nil {
		test.Fatal(err)
	}
	if page, err := service.QueryPreparedMemory(ctx, repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
		test.Fatal("invalidated pin became an endless retry", page, err)
	}
}
