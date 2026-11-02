package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestEvidenceQueriesKeepBoundSourceWithoutServiceBackReference(test *testing.T) {
	fixture := newPRMemoryFixture(test)
	fixture.publish(test, fixture.digest(test, "", "bound evidence"), true)
	fixture.reconcile(test)
	service := fixture.worker.core
	history, err := service.ListHistory(systemTestContext(), fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	queries, err := service.evidenceQueries()
	if err != nil {
		test.Fatal(err)
	}
	service.meta = store.NewFSStore(test.TempDir())
	actual, err := queries.VerifiedPR(systemTestContext(), fixture.repo.ID, history)
	if err != nil || len(actual) != 1 || !actual[0].Valid || len(actual[0].Publications) != 1 || actual[0].Record.RepoID != fixture.repo.ID {
		test.Fatal("query rediscovered the parent Service or lost bound verification", actual, err)
	}
}
