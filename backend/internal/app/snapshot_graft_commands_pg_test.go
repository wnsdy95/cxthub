//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type snapshotGraftFailurePG struct {
	*store.PostgresStore
	failure error
	writes  int
}

func (storage *snapshotGraftFailurePG) AddGraftParentsCAS(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, expected uint64) error {
	if err := storage.PostgresStore.AddGraftParentsCAS(ctx, repo, id, parents, expected); err != nil {
		return err
	}
	storage.writes++
	return storage.failure
}

func TestSnapshotGraftCommandsPGRollback(test *testing.T) {
	fixture := newRepositoryRefFixturePG(test)
	storage := &snapshotGraftFailurePG{PostgresStore: fixture.storage, failure: errors.New("after actual graft CAS")}
	service := NewService(storage, storage, nil, nil, storage)
	commands, err := service.SnapshotGraftCommands()
	if err != nil {
		test.Fatal(err)
	}
	*service = Service{}
	before := fixture.image(test)
	err = commands.GraftSnapshotParents(repositoryRefActorPG(fixture), fixture.repo, fixture.orphan, []domain.ContentHash{fixture.base}, 0)
	if !errors.Is(err, storage.failure) || storage.writes != 1 {
		test.Fatal("injected failure did not follow the actual SQL CAS", storage.writes, err)
	}
	if after := fixture.image(test); !reflect.DeepEqual(before, after) {
		test.Fatal("failed graft left snapshots, revision, history, pending, notifications, or audit changes", before, after)
	}
	storage.failure = nil
	if err := commands.GraftSnapshotParents(repositoryRefActorPG(fixture), fixture.repo, fixture.orphan, []domain.ContentHash{fixture.base}, 0); err != nil {
		test.Fatal("valid caller could not publish after rollback", err)
	}
	actual, err := storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.orphan)
	if err != nil || actual.GraftSeq != 1 || !reflect.DeepEqual(actual.GraftParents, []domain.ContentHash{fixture.base}) {
		test.Fatal(actual, err)
	}
}

func TestSnapshotGraftCommandsPGConcurrentCAS(test *testing.T) {
	fixture := newRepositoryRefFixturePG(test)
	peer, _ := repositoryRefPeerPG(test, fixture.ctx, fixture.observer, fixture.repo, "graft")
	owners := make([]*SnapshotGraftCommands, 0, 2)
	for _, storage := range []*store.PostgresStore{fixture.storage, peer} {
		service := NewService(storage, storage, nil, nil, storage)
		commands, err := service.SnapshotGraftCommands()
		if err != nil {
			test.Fatal(err)
		}
		*service = Service{}
		owners = append(owners, commands)
	}
	before := fixture.image(test)
	parents := []domain.ContentHash{fixture.base, fixture.loose}
	type result struct {
		index int
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for index, commands := range owners {
		go func() {
			<-start
			results <- result{index: index, err: commands.GraftSnapshotParents(repositoryRefActorPG(fixture), fixture.repo, fixture.orphan, []domain.ContentHash{parents[index]}, 0)}
		}()
	}
	close(start)
	winner, conflicts := -1, 0
	for range owners {
		select {
		case completed := <-results:
			if completed.err == nil {
				if winner != -1 {
					test.Fatal("two owners published with the same expected sequence")
				}
				winner = completed.index
			} else if errors.Is(completed.err, domain.ErrConflict) {
				conflicts++
			} else {
				test.Fatal(completed.err)
			}
		case <-fixture.ctx.Done():
			test.Fatal(fixture.ctx.Err())
		}
	}
	if winner < 0 || conflicts != 1 {
		test.Fatal("concurrent grafts did not have exactly one winner", winner, conflicts)
	}
	actual, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.orphan)
	if err != nil || actual.GraftSeq != 1 || !reflect.DeepEqual(actual.GraftParents, []domain.ContentHash{parents[winner]}) || len(actual.Parents) != 0 {
		test.Fatal("CAS loser changed the overlay", actual, err)
	}
	after := fixture.image(test)
	if after.revision.Graph != before.revision.Graph+1 || after.revision.Pending != before.revision.Pending || after.revision.Evidence != before.revision.Evidence ||
		!reflect.DeepEqual(after.refs, before.refs) || !reflect.DeepEqual(after.history, before.history) || !reflect.DeepEqual(after.documents, before.documents) ||
		!reflect.DeepEqual(after.pending, before.pending) || !reflect.DeepEqual(after.notifications, before.notifications) || len(after.audit) != len(before.audit)+1 {
		test.Fatal("concurrent graft changed unrelated state or published multiple revisions", before, after)
	}
}
