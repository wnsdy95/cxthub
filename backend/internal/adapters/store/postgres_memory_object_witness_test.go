//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryWitnessPGCapture(test *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) domain.CapturedMemoryObject {
	test.Helper()
	var object domain.CapturedMemoryObject
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		var err error
		object, err = storage.CaptureMemoryObject(bound, repo, hash, 2<<20)
		return err
	}); err != nil {
		test.Fatal(err)
	}
	return object
}

func memoryWitnessPGCheck(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, witnesses []domain.MemoryObjectWitness, wanted error) {
	test.Helper()
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		return storage.ValidateMemoryObjectWitnesses(bound, repo, witnesses)
	})
	if !errors.Is(err, wanted) {
		test.Fatalf("object currency: got %v want %v", err, wanted)
	}
}

func TestPGMemoryObjectWitnessRootComponentsAndRepresentation(test *testing.T) {
	for _, chunked := range []bool{false, true} {
		test.Run(map[bool]string{false: "legacy", true: "chunked"}[chunked], func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			digest := memoryCaptureTestDigest()
			digest.SnapshotID = repo
			if chunked {
				digest.Summary = strings.Repeat("bounded witness memory "+string(repo), 2500)
			}
			hash, err := storage.PutMemory(ctx, repo, digest)
			if err != nil {
				test.Fatal(err)
			}
			object := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
			if object.Digest == nil || !reflect.DeepEqual(*object.Digest, digest) || object.Witness.Validate() != nil || !object.Witness.Available || chunked && len(object.Witness.Components) == 0 {
				test.Fatal("witness changed verified content", object.Witness)
			}
			memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{object.Witness}, nil)
			before := memorySourceGenerationPG(test, ctx, storage, repo)
			changed := hash
			if chunked {
				changed = object.Witness.Components[0].Hash
			}
			var raw []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(changed)).Scan(&raw); err != nil {
				test.Fatal(err)
			}
			defer func() {
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(changed), raw); err != nil {
					test.Error("restore deliberately corrupted fixture", err)
				}
			}()
			decoded, err := docDecompress(raw)
			if err != nil {
				test.Fatal(err)
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(changed), docCompress(decoded))
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{object.Witness}, domain.ErrConflict)
			recaptured := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
			if recaptured.Digest == nil || !reflect.DeepEqual(*recaptured.Digest, digest) {
				test.Fatal("representation rewrite changed logical memory")
			}
			memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{recaptured.Witness}, nil)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=decode('00','hex') WHERE hash=$1`, string(changed))
			memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{recaptured.Witness}, domain.ErrConflict)
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				actual, err := storage.CaptureMemoryObject(bound, repo, hash, 2<<20)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(actual, domain.CapturedMemoryObject{}) {
					test.Fatal("corruption issued witness", err)
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestPGMemoryObjectWitnessAbsenceOwnershipAndABA(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	foreign := memorySourceRepoPG(test, ctx, storage, "witness foreign")
	absent := memoryWitnessPGCapture(test, storage, ctx, foreign, hash)
	if absent.Digest != nil || absent.Witness.Available || absent.Witness.Root != nil {
		test.Fatal("foreign grant reused")
	}
	memoryWitnessPGCheck(test, storage, ctx, foreign, []domain.MemoryObjectWitness{absent.Witness}, nil)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(foreign), string(hash))
	memoryWitnessPGCheck(test, storage, ctx, foreign, []domain.MemoryObjectWitness{absent.Witness}, domain.ErrConflict)
	original := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	var raw []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&raw); err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `DELETE FROM repo_blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, transaction, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, transaction, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), raw)
	catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(hash))
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{original.Witness}, domain.ErrConflict)
	current := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	if current.Witness.Root.Version != 0 || current.Witness.Root.Incarnation == original.Witness.Root.Incarnation {
		test.Fatal("delete/reinsert reused physical identity")
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash))
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{current.Witness}, domain.ErrConflict)
}

func TestPGMemoryObjectWitnessRollbackAndSharedStaleWriter(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	original := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	rollback := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, rollback, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if err := rollback.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{original.Witness}, nil)
	writer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(writer)
	foreign := memorySourceRepoPG(test, ctx, storage, "shared witness")
	var exists bool
	if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND hash=$2)`, string(foreign), string(hash)).Scan(&exists); err != nil || exists {
		test.Fatal("writer unexpectedly saw future grant", err)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(foreign), string(hash))
	shared := memoryWitnessPGCapture(test, storage, ctx, foreign, hash)
	before := memorySourceGenerationPG(test, ctx, storage, foreign)
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	memorySourceAdvancePG(test, ctx, storage, before, 0)
	memoryWitnessPGCheck(test, storage, ctx, foreign, []domain.MemoryObjectWitness{shared.Witness}, domain.ErrConflict)
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{original.Witness}, domain.ErrConflict)
}

func TestPGMemoryObjectWitnessScopesAndMalformedInputs(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	hash := domain.HashContent([]byte("missing"))
	if _, err := storage.CaptureMemoryObject(ctx, repo, hash, 1024); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("unscoped capture", err)
	}
	if err := storage.ValidateMemoryObjectWitnesses(ctx, repo, nil); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("unscoped validation", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.CaptureMemoryObject(bound, repo, hash, 1024); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("write capture", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		peer := &PostgresStore{pool: storage.pool}
		if _, err := peer.CaptureMemoryObject(bound, repo, hash, 1024); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("foreign store", err)
		}
		canceled, cancel := context.WithCancel(bound)
		cancel()
		if _, err := storage.CaptureMemoryObject(canceled, repo, hash, 1024); !errors.Is(err, context.Canceled) {
			test.Fatal("cancellation", err)
		}
		absent := domain.MemoryObjectWitness{RepoID: repo, Hash: hash}
		for _, witnesses := range [][]domain.MemoryObjectWitness{{{}}, {absent, absent}, {{RepoID: domain.HashContent([]byte("other")), Hash: hash}}} {
			if err := storage.ValidateMemoryObjectWitnesses(bound, repo, witnesses); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("malformed witness accepted", err)
			}
		}
		if err := storage.ValidateMemoryObjectWitnesses(bound, repo, make([]domain.MemoryObjectWitness, domain.MaxMemoryCaptureObjects+1)); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
			test.Fatal("unbounded witnesses", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}
