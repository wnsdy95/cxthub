//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryQualificationPG(test *testing.T) (*PostgresStore, context.Context, domain.MemoryReadQualification, domain.MemoryReadBundle) {
	test.Helper()
	return memoryQualificationSizedPG(test, 0)
}

func memoryQualificationSizedPG(test *testing.T, unrelated int) (*PostgresStore, context.Context, domain.MemoryReadQualification, domain.MemoryReadBundle) {
	test.Helper()
	storage, ctx, repo := catalogPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	object := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	if unrelated > 0 {
		transaction := catalogTxPG(test, ctx, storage)
		catalogExecPG(test, ctx, transaction, `INSERT INTO blobs(hash,bytes) SELECT 'sha256:' || repeat(md5($1 || ordinal::text),2),convert_to(repeat('unrelated payload',1024),'UTF8') FROM generate_series(1,$2::int) ordinal`, string(repo), unrelated)
		catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) SELECT $1,'memory','sha256:' || repeat(md5($1 || ordinal::text),2) FROM generate_series(1,$2::int) ordinal`, string(repo), unrelated)
		if err := transaction.Commit(ctx); err != nil {
			test.Fatal(err)
		}
	}
	capture, output := memoryReadPGFixture(test, storage, ctx, repo, 3)
	header := output.Header
	header.Dependencies = []domain.MemoryReadDependency{{Hash: hash, Available: true}}
	var items []domain.EffectiveMemoryItem
	for ordinal, chunk := range output.Chunks {
		decoded, err := domain.DecodeMemoryReadChunk(ctx, output.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		items = append(items, decoded...)
	}
	output = memoryReadPGSeal(test, ctx, header, items)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, output)
	qualification := domain.MemoryReadQualification{Version: domain.MemoryReadQualificationVersion, ReadID: output.ID,
		CaptureID: capture.ID, Source: output.Header.Source, Witnesses: []domain.MemoryObjectWitness{object.Witness}}
	return storage, ctx, qualification, output
}

func memoryQualificationCountPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) int {
	test.Helper()
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_qualifications WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil {
		test.Fatal(err)
	}
	return count
}

func memoryQualificationAllowPG(ctx context.Context) (context.Context, error) {
	return ctx, ctx.Err()
}

func TestPGMemoryQualificationScopesBindingsAndReadLifetime(test *testing.T) {
	storage, ctx, qualification, output := memoryQualificationPG(test)
	repo := qualification.Source.Graph.RepoID
	identifier, err := storage.QualifyMemoryRead(ctx, qualification)
	if err != nil {
		test.Fatal(err)
	}
	var escaped outbound.MemoryReadChunkReader
	var escapedContext context.Context
	if err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
		escaped, escapedContext = chunks, bound
		if !reflect.DeepEqual(header, output.Header) {
			test.Fatal("qualified header changed")
		}
		_, err := chunks.ReadChunk(bound, 0)
		return err
	}); err != nil {
		test.Fatal(err)
	}
	if _, err := escaped.ReadChunk(escapedContext, 0); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("qualified reader survived its snapshot", err)
	}
	if _, err := storage.QualifyMemoryRead(outbound.WithDocReadOnly(ctx), qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("read-only marker published qualification", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := storage.QualifyMemoryRead(bound, qualification); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("publication accepted borrowed read", err)
		}
		if err := storage.WithQualifiedMemoryReadRange(bound, repo, identifier, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error { return nil }); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("current read accepted borrowed snapshot", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"missing witnesses", "foreign capture", "wrong source", "wrong repo"} {
		test.Run(mode, func(test *testing.T) {
			altered := qualification
			switch mode {
			case "missing witnesses":
				altered.Witnesses = nil
			case "foreign capture":
				altered.CaptureID = domain.HashContent([]byte("foreign"))
			case "wrong source":
				altered.Source.Sequence++
			case "wrong repo":
				altered.Source.Graph.RepoID = domain.HashContent([]byte("foreign"))
			}
			if id, err := storage.QualifyMemoryRead(ctx, altered); err == nil || id != "" {
				test.Fatal("substituted qualification association accepted", err)
			}
		})
	}
	if count := memoryQualificationCountPG(test, storage, ctx, repo); count != 1 {
		test.Fatal("invalid qualification left side effects", count)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE memory_read_qualifications SET body=body WHERE repo_id=$1`, string(repo)); err == nil {
		test.Fatal("qualification could be edited")
	}
	memoryBundlePGExpire(test, storage, ctx, repo, qualification.CaptureID)
	called := false
	if err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
		called = true
		return nil
	}); !errors.Is(err, domain.ErrMemoryCaptureExpired) || called {
		test.Fatal("expired qualification exposed a reader", err)
	}
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 || memoryQualificationCountPG(test, storage, ctx, repo) != 0 {
		test.Fatal("prune retained orphan qualification", count, err)
	}
}

func TestPGMemoryQualificationRollbackAndCorruption(test *testing.T) {
	storage, ctx, qualification, _ := memoryQualificationPG(test)
	repo := qualification.Source.Graph.RepoID
	raw, err := domain.MemoryReadQualificationBytes(ctx, qualification)
	if err != nil {
		test.Fatal(err)
	}
	identifier := domain.HashContent(raw)
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `INSERT INTO memory_read_qualifications(repo_id,qualification_id,read_id,body) VALUES($1,$2,$3,$4)`, string(repo), string(identifier), string(qualification.ReadID), raw)
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if memoryQualificationCountPG(test, storage, ctx, repo) != 0 {
		test.Fatal("rolled-back qualification became visible")
	}
	if _, err := storage.QualifyMemoryRead(ctx, qualification); err != nil {
		test.Fatal(err)
	}
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_qualifications SET body=convert_to('{}','UTF8') WHERE repo_id=$1`, string(repo))
	if _, err := storage.QualifyMemoryRead(ctx, qualification); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("duplicate publication repaired corrupt qualification", err)
	}
	called := false
	if err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
		called = true
		return nil
	}); !errors.Is(err, domain.ErrIntegrity) || called {
		test.Fatal("corrupt qualification exposed a reader", err)
	}
}
