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

type memoryDigestFailurePG struct {
	*store.PostgresStore
	failure error
	writes  int
}

func (storage *memoryDigestFailurePG) CompareAndSwapSnapshotMemory(ctx context.Context, repo, target, expected, next domain.ContentHash) error {
	if err := storage.PostgresStore.CompareAndSwapSnapshotMemory(ctx, repo, target, expected, next); err != nil {
		return err
	}
	storage.writes++
	return storage.failure
}

func TestMemoryDigestCommandsPGRollbackAndBorrowedTransaction(test *testing.T) {
	for _, nested := range []bool{false, true} {
		test.Run(map[bool]string{false: "separate writes", true: "borrowed transaction"}[nested], func(test *testing.T) {
			fixture := newRepositoryRefFixturePG(test)
			storage := &memoryDigestFailurePG{PostgresStore: fixture.storage, failure: errors.New("after actual memory CAS")}
			service := NewService(storage, storage, nil, nil, storage)
			commands, err := service.MemoryDigestCommands()
			if err != nil {
				test.Fatal(err)
			}
			*service = Service{}
			digest := domain.MemoryDigest{SnapshotID: fixture.orphan, Summary: "rollback memory"}
			memory, err := domain.MemoryDigestHash(digest)
			if err != nil {
				test.Fatal(err)
			}
			before := fixture.image(test)
			write := func(bound context.Context) (domain.ContentHash, error) {
				return commands.PutMemoryDigestCAS(bound, fixture.repo, digest)
			}
			var hash domain.ContentHash
			if nested {
				hash, err = repositoryWriteWithWork(repositoryRefActorPG(fixture), commands.scope.(*memoryDigestScope).work, fixture.repo, write)
			} else {
				hash, err = write(repositoryRefActorPG(fixture))
			}
			if hash != "" || !errors.Is(err, storage.failure) || storage.writes != 1 {
				test.Fatal("failure did not follow actual SQL CAS", hash, err, storage.writes)
			}
			after := fixture.image(test)
			retained, readErr := fixture.storage.GetMemory(fixture.ctx, fixture.repo, memory)
			if nested {
				if !errors.Is(readErr, domain.ErrNotFound) {
					test.Fatal("borrowed transaction retained rolled-back blob", readErr)
				}
			} else {
				if readErr != nil || retained.Summary != digest.Summary || len(after.audit) != len(before.audit)+1 || repositoryRefAuditCountPG(after.audit, "context.memory.updated") != repositoryRefAuditCountPG(before.audit, "context.memory.updated")+1 {
					test.Fatal("separate blob commit or original audit was lost", readErr, before.audit, after.audit)
				}
				before.audit = after.audit
			}
			if !reflect.DeepEqual(before, after) {
				test.Fatal("failed pointer write changed repository state", before, after)
			}
			storage.failure = nil
			if hash, err := commands.PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, digest); err != nil || hash != memory {
				test.Fatal("retry after rollback failed", hash, err)
			}
		})
	}
}

func TestMemoryDigestCommandsPGConcurrentCASRetainsBothObjects(test *testing.T) {
	fixture := newRepositoryRefFixturePG(test)
	peer, _ := repositoryRefPeerPG(test, fixture.ctx, fixture.observer, fixture.repo, "memory")
	owners := make([]*MemoryDigestCommands, 0, 2)
	for _, storage := range []*store.PostgresStore{fixture.storage, peer} {
		service := NewService(storage, storage, nil, nil, storage)
		commands, err := service.MemoryDigestCommands()
		if err != nil {
			test.Fatal(err)
		}
		*service = Service{}
		owners = append(owners, commands)
	}
	root, err := owners[0].PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, domain.MemoryDigest{SnapshotID: fixture.orphan, ClaimsVersion: 1})
	if err != nil {
		test.Fatal(err)
	}
	digests := []domain.MemoryDigest{
		{SnapshotID: fixture.orphan, PreviousMemoryHash: root, ClaimsVersion: 1, Summary: "left"},
		{SnapshotID: fixture.orphan, PreviousMemoryHash: root, ClaimsVersion: 1, Summary: "right"},
	}
	before := fixture.image(test)
	type result struct {
		index int
		hash  domain.ContentHash
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for index, commands := range owners {
		go func() {
			<-start
			hash, err := commands.PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, digests[index])
			results <- result{index: index, hash: hash, err: err}
		}()
	}
	close(start)
	winner, conflicts := -1, 0
	for range owners {
		select {
		case completed := <-results:
			if completed.err == nil {
				if winner != -1 || completed.hash == "" {
					test.Fatal("concurrent CAS produced multiple/empty successes", completed)
				}
				winner = completed.index
			} else if errors.Is(completed.err, domain.ErrConflict) && completed.hash == "" {
				conflicts++
			} else {
				test.Fatal(completed)
			}
		case <-fixture.ctx.Done():
			test.Fatal(fixture.ctx.Err())
		}
	}
	if winner < 0 || conflicts != 1 {
		test.Fatal("CAS did not have exactly one winner", winner, conflicts)
	}
	for _, digest := range digests {
		hash, err := domain.MemoryDigestHash(digest)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := peer.GetMemory(fixture.ctx, fixture.repo, hash)
		if err != nil || retained.Summary != digest.Summary {
			test.Fatal("concurrent blob not retained", retained, err)
		}
	}
	winnerHash, err := domain.MemoryDigestHash(digests[winner])
	if err != nil {
		test.Fatal(err)
	}
	current, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.orphan)
	if err != nil || current.MemoryHash != winnerHash {
		test.Fatal("loser changed snapshot pointer", current, err)
	}
	after := fixture.image(test)
	if after.revision.Graph != before.revision.Graph+1 || after.revision.Pending != before.revision.Pending || after.revision.Evidence != before.revision.Evidence ||
		!reflect.DeepEqual(after.refs, before.refs) || !reflect.DeepEqual(after.history, before.history) || !reflect.DeepEqual(after.documents, before.documents) ||
		!reflect.DeepEqual(after.pending, before.pending) || !reflect.DeepEqual(after.notifications, before.notifications) || len(after.audit) != len(before.audit)+3 {
		test.Fatal("concurrent memory changed unrelated state or write boundaries", before, after)
	}
	if replay, err := owners[winner].PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, digests[winner]); err != nil || replay != winnerHash {
		test.Fatal("immediate winner replay failed", replay, err)
	}
}

func TestMemoryDigestCommandsPGCurrentAuthorityBetweenWrites(test *testing.T) {
	for _, mutation := range []string{"direct", "team-grant", "organization-owner", "organization-policy"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newRepositoryRefFixturePG(test)
			repositoryAuthorityPrepareInheritancePG(test, fixture.ctx, fixture.storage, fixture.authorityScopeFixture, mutation)
			service := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
			commands, err := service.MemoryDigestCommands()
			if err != nil {
				test.Fatal(err)
			}
			*service = Service{}
			var revoked repositoryRefImagePG
			commands.scope = &memoryDigestAfterObjectScope{MemoryDigestWriteScope: commands.scope, after: func() error {
				if err := repositoryAuthorityRevokePG(fixture.ctx, fixture.team.identity, fixture.authorityScopeFixture, mutation); err != nil {
					return err
				}
				revoked = fixture.image(test)
				return nil
			}}
			digest := domain.MemoryDigest{SnapshotID: fixture.orphan, Summary: "durable before authority revocation"}
			memory, err := domain.MemoryDigestHash(digest)
			if err != nil {
				test.Fatal(err)
			}
			if hash, err := commands.PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, digest); hash != "" || !errors.Is(err, domain.ErrForbidden) {
				test.Fatal("pointer write retained revoked authority", hash, err)
			}
			if retained, err := fixture.storage.GetMemory(fixture.ctx, fixture.repo, memory); err != nil || retained.Summary != digest.Summary {
				test.Fatal("earlier committed blob lost", retained, err)
			}
			current, err := fixture.storage.GetSnapshot(fixture.ctx, fixture.repo, fixture.orphan)
			if err != nil || current.MemoryHash != "" || !reflect.DeepEqual(revoked, fixture.image(test)) {
				test.Fatal("revoked pointer write changed durable state", current, err)
			}
			if hash, err := commands.PutMemoryDigestCAS(repositoryRefActorPG(fixture), fixture.repo, digest); hash != "" || !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(revoked, fixture.image(test)) {
				test.Fatal("reused owner retained revoked authority", hash, err)
			}
		})
	}
}
