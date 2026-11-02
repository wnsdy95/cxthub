//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMemoryMinimumProgressDuringRepeatedWritesAndPinnedPages(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	request.Limit = 1
	memoryWorkConfigurePG(test, service, storage, repo)
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	var expected []domain.EffectiveMemoryPage
	before := request
	for {
		page, err := service.QueryEffectiveMemory(ctx, repo, before)
		if err != nil {
			test.Fatal(err)
		}
		expected = append(expected, page)
		if page.NextCursor == "" {
			break
		}
		if len(expected) > 30 {
			test.Fatal("fixture did not terminate")
		}
		before.Cursor = page.NextCursor
	}
	if len(expected) < 2 {
		test.Fatal("fixture needs multiple pages")
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	probe := &memoryWorkProbePG{PostgresStore: storage}
	service.meta = probe
	var first domain.MemoryReadPage
	for iteration := 0; iteration < 5; iteration++ {
		probe.beforeFinish = func() {
			if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' concurrent write' WHERE repo_id=$1`, repo); err != nil {
				test.Fatal(err)
			}
		}
		if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
			test.Fatal("later logical write starved publication", iteration, err)
		}
		page, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
		if err != nil || !page.Status.Complete || page.Status.Freshness != "catching_up" || page.Status.Minimum != requirement.Minimum {
			test.Fatal("fixed-floor request did not progress", iteration, page.Status, err)
		}
		if iteration == 0 {
			first = page
			actual, want := page.Page, expected[0]
			actual.NextCursor, want.NextCursor = "", ""
			if !reflect.DeepEqual(actual, want) {
				test.Fatal("bounded result differs from captured pre-write facts")
			}
		}
		if _, err := service.QueryCurrentMemory(ctx, repo, request); !errors.Is(err, domain.ErrMemoryReadNotReady) {
			test.Fatal("bounded result was falsely labeled exact-current", err)
		}
		newer, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := service.QueryMinimumMemory(ctx, repo, request, newer, ""); !errors.Is(err, domain.ErrMemoryReadNotReady) {
			test.Fatal("result older than newly observed floor was accepted", err)
		}
		var pending bool
		if err := connection.QueryRow(ctx, `SELECT wake.revision>job.wake_revision AND wake.next_attempt<=clock_timestamp()
 FROM memory_read_wakes wake JOIN memory_read_work job USING(repo_id)
 WHERE job.repo_id=$1 AND job.target_id=$2`, repo, requirement.TargetID).Scan(&pending); err != nil || !pending {
			test.Fatal("bounded completion consumed a later source notification", pending, err)
		}
	}
	continuation := request
	continuation.Cursor = first.Page.NextCursor
	for ordinal := 1; ordinal < len(expected); ordinal++ {
		page, err := service.QueryMinimumMemory(ctx, repo, continuation, requirement, first.Status.Generation)
		if err != nil || page.Status.Generation != first.Status.Generation || page.Status.Applied != first.Status.Applied {
			test.Fatal("continuation followed a mutable latest pointer", ordinal, err)
		}
		actual, want := page.Page, expected[ordinal]
		actual.NextCursor, want.NextCursor = "", ""
		if !reflect.DeepEqual(actual, want) {
			test.Fatal("continuation spliced another generation", ordinal)
		}
		continuation.Cursor = page.Page.NextCursor
	}
	if continuation.Cursor != "" {
		test.Fatal("pinned continuation did not terminate")
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	latest, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
	if err != nil || latest.Status.Freshness != "current" {
		test.Fatal("quiet-source completion did not catch up", latest.Status, err)
	}
	continuation.Cursor = first.Page.NextCursor
	if _, err := service.QueryMinimumMemory(ctx, repo, continuation, requirement, latest.Status.Generation); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
		test.Fatal("cursor crossed generations", err)
	}
	if _, err := service.QueryMinimumMemory(ctx, repo, continuation, requirement, ""); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("continuation omitted its generation", err)
	}
	if _, err := service.QueryCurrentMemory(ctx, repo, request); err != nil {
		test.Fatal("strict reader failed after catching up", err)
	}
}

type minimumMemoryClaimProbePG struct {
	*store.PostgresStore
	claimed domain.MemoryReadWork
	after   func()
}

func (probe *minimumMemoryClaimProbePG) ClaimMemoryReadWork(ctx context.Context, repos []domain.ContentHash, lease time.Duration) (domain.MemoryReadWork, error) {
	job, err := probe.PostgresStore.ClaimMemoryReadWork(ctx, repos, lease)
	if err == nil {
		probe.claimed = job
		probe.after()
	}
	return job, err
}

func TestPGMemoryMinimumCaptureCanAdvanceBeyondClaimFloor(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	probe := &minimumMemoryClaimProbePG{PostgresStore: storage, after: func() {
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=message || ' before capture' WHERE repo_id=$1`, repo); err != nil {
			test.Fatal(err)
		}
	}}
	service.meta = probe
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("capture unnecessarily required the exact claim generation", err)
	}
	completed, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil || completed.State != "completed" || completed.Source == probe.claimed.Source ||
		domain.CheckMemorySourceMinimum(completed.Source, probe.claimed.Source) != nil {
		test.Fatal("completion did not preserve its newer applied source", completed, err)
	}
	assertCurrentMemoryParityPG(test, service, repo, request)
}

func TestPGMemoryMinimumAuthorityAndErrorsReturnNoPartialPage(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	requirement, err := service.ObserveMemoryReadRequirement(ctx, repo, request)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := service.RequestMemoryReadPreparation(ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal(err)
	}
	request.Limit = 1
	first, err := service.QueryMinimumMemory(ctx, repo, request, requirement, "")
	if err != nil || first.Page.NextCursor == "" {
		test.Fatal("fixture needs a continuation", err)
	}
	request.Cursor = first.Page.NextCursor
	denied := inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
		return nil, domain.ErrForbidden
	})
	for _, sample := range []struct {
		ctx  context.Context
		want error
	}{
		{context.Background(), domain.ErrCatalogUnsupported},
		{denied, domain.ErrForbidden},
	} {
		if page, err := service.QueryMinimumMemory(sample.ctx, repo, request, requirement, first.Status.Generation); !errors.Is(err, sample.want) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
			test.Fatal("continuation reused old authority", page, err)
		}
		observation := request
		observation.Cursor = ""
		if observed, err := service.ObserveMemoryReadRequirement(sample.ctx, repo, observation); !errors.Is(err, sample.want) || observed != (domain.MemoryReadRequirement{}) {
			test.Fatal("observation reused old authority", observed, err)
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		page, err := service.QueryMinimumMemory(bound, repo, request, requirement, first.Status.Generation)
		if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
			test.Fatal("continuation borrowed a stale snapshot", page, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, request.Selection.MemoryHash); err != nil {
		test.Fatal(err)
	}
	if page, err := service.QueryMinimumMemory(ctx, repo, request, requirement, first.Status.Generation); !errors.Is(err, domain.ErrMemoryReadNotReady) || !reflect.DeepEqual(page, domain.MemoryReadPage{}) {
		test.Fatal("changed available object returned a partial page", page, err)
	}
}
