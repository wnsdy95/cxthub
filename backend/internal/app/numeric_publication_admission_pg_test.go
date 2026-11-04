//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type numericPublicationPGStore struct {
	*store.PostgresStore
	writes          map[string]int
	unscopedWrites  int
	rejectSnapshot  domain.ContentHash
	snapshotFailure error
}

func (storage *numericPublicationPGStore) recordWrite(ctx context.Context, operation string) error {
	storage.writes[operation]++
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) {
		storage.unscopedWrites++
		return domain.ErrConflict
	}
	return nil
}

func (storage *numericPublicationPGStore) PutDoc(ctx context.Context, repo domain.ContentHash, document domain.SessionDoc) (bool, error) {
	if err := storage.recordWrite(ctx, "doc"); err != nil {
		return false, err
	}
	return storage.PostgresStore.PutDoc(ctx, repo, document)
}

func (storage *numericPublicationPGStore) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, document domain.VerifiedSessionDoc) (bool, error) {
	if err := storage.recordWrite(ctx, "verified_doc"); err != nil {
		return false, err
	}
	return storage.PostgresStore.PutVerifiedDoc(ctx, repo, document)
}

func (storage *numericPublicationPGStore) PutSnapshot(ctx context.Context, snapshot domain.Snapshot) error {
	if err := storage.recordWrite(ctx, "snapshot"); err != nil {
		return err
	}
	if snapshot.ID == storage.rejectSnapshot {
		return storage.snapshotFailure
	}
	return storage.PostgresStore.PutSnapshot(ctx, snapshot)
}

func (storage *numericPublicationPGStore) CompareAndSwapRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	if err := storage.recordWrite(ctx, "ref"); err != nil {
		return err
	}
	return storage.PostgresStore.CompareAndSwapRef(ctx, repo, ref, expected)
}

func (storage *numericPublicationPGStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.recordWrite(ctx, "revision"); err != nil {
		return err
	}
	return storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending)
}

func (storage *numericPublicationPGStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := storage.recordWrite(ctx, "audit"); err != nil {
		return err
	}
	return storage.PostgresStore.AppendOrganizationAudit(ctx, event)
}

func numericPublicationPGObjectCounts(test *testing.T, observer *pgxpool.Pool, repo domain.ContentHash, documents []domain.SessionDoc) [4]int {
	test.Helper()
	hashes := numericPublicationObjectHashes(test, documents)
	values := make([]string, len(hashes))
	for position, hash := range hashes {
		values[position] = string(hash)
	}
	var counts [4]int
	if err := observer.QueryRow(systemTestContext(), `SELECT
		(SELECT count(*) FROM blobs WHERE hash=ANY($1::text[])),
		(SELECT count(*) FROM repo_blobs WHERE repo_id=$2),
		(SELECT count(*) FROM doc_read_indexes_v3 WHERE hash=ANY($1::text[])),
		(SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=ANY($1::text[]))`, values, string(repo)).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		test.Fatal(err)
	}
	return counts
}

func TestPGNumericPublicationAdmissionPublicCommit(test *testing.T) {
	_, storage, repo := collaborationPG(test)
	guard := &numericPublicationPGStore{PostgresStore: storage, writes: make(map[string]int)}
	service := NewService(guard, guard, nil, gitengine.NewEngine(guard), guard)
	fixture := newNumericPublicationFixture(test, service, storage, repo)
	observer, err := pgxpool.New(systemTestContext(), collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(observer.Close)
	if guard.writes["verified_doc"] != 1 || guard.writes["snapshot"] != 1 || guard.writes["ref"] == 0 || guard.writes["audit"] < 2 || guard.unscopedWrites != 0 {
		test.Fatalf("public seed did not exercise native scoped publication: writes=%v unscoped=%d", guard.writes, guard.unscopedWrites)
	}
	for _, location := range []string{"tool_input", "tool_output", "nested_compaction"} {
		for _, layout := range []string{"invalid_first", "valid_prefix", "duplicate_claim"} {
			test.Run(location+"/"+layout, func(test *testing.T) {
				input := numericPublicationRejectedInput(test, fixture, location, layout)
				before := numericPublicationState(test, fixture)
				objects := numericPublicationPGObjectCounts(test, observer, repo, input.Docs)
				guard.writes = make(map[string]int)
				output, err := service.Commit(fixture.ctx, input)
				if !errors.Is(err, domain.ErrIntegrity) || output != (inbound.CommitOutput{}) {
					test.Errorf("public PostgreSQL numeric admission: output=%+v err=%v", output, err)
				}
				if len(guard.writes) != 0 || guard.unscopedWrites != 0 {
					test.Errorf("rejection reached publication before full validation: writes=%v unscoped=%d", guard.writes, guard.unscopedWrites)
				}
				if after := numericPublicationState(test, fixture); after != before {
					test.Errorf("failed public commit changed durable repository/audit state:\nbefore=%s\nafter=%s", before, after)
				}
				if after := numericPublicationPGObjectCounts(test, observer, repo, input.Docs); after != objects {
					test.Errorf("failed public commit leaked global CAS blobs or ownership: before=%v after=%v", objects, after)
				}
				numericPublicationAssertAbsent(test, fixture, input.Docs)
			})
		}
	}
	test.Run("native_rollback_after_accepted_numeric_writes", func(test *testing.T) {
		first := numericPublicationCompatibleDocument(test, string(repo)+"/rollback/first", "1787683260.123456789")
		second := numericPublicationCompatibleDocument(test, string(repo)+"/rollback/second", "1e999")
		input := inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{first, second}, Snapshots: []domain.Snapshot{
			numericPublicationSnapshot(repo, first), numericPublicationSnapshot(repo, second),
		}}
		before := numericPublicationState(test, fixture)
		objects := numericPublicationPGObjectCounts(test, observer, repo, input.Docs)
		failure := errors.New("numeric admission rollback after first native snapshot")
		guard.rejectSnapshot, guard.snapshotFailure = second.Hash, failure
		guard.writes = make(map[string]int)
		defer func() { guard.rejectSnapshot, guard.snapshotFailure = "", nil }()
		output, err := service.Commit(fixture.ctx, input)
		if !errors.Is(err, failure) || output != (inbound.CommitOutput{}) {
			test.Errorf("public rollback did not reach second snapshot: output=%+v err=%v", output, err)
		}
		if guard.writes["verified_doc"] != 2 || guard.writes["snapshot"] != 2 || guard.writes["audit"] != 0 || guard.writes["ref"] != 0 || guard.unscopedWrites != 0 {
			test.Errorf("rollback did not perform real scoped document/first-snapshot writes: %v unscoped=%d", guard.writes, guard.unscopedWrites)
		}
		if after := numericPublicationState(test, fixture); after != before {
			test.Errorf("native rollback changed existing repository/audit state:\nbefore=%s\nafter=%s", before, after)
		}
		if after := numericPublicationPGObjectCounts(test, observer, repo, input.Docs); after != objects {
			test.Errorf("native rollback leaked global CAS blobs or grants: before=%v after=%v", objects, after)
		}
		numericPublicationAssertAbsent(test, fixture, input.Docs)
	})
	test.Run("compatible_publication_and_reload", func(test *testing.T) {
		runNumericPublicationRoundTrips(test, service, storage, fixture.ctx, repo, false)
		if guard.unscopedWrites != 0 {
			test.Fatalf("accepted public numeric commit escaped repository transaction: %d", guard.unscopedWrites)
		}
	})
}
