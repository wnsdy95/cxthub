//go:build postgres

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryQualificationPausePG(test *testing.T, ctx context.Context, storage *PostgresStore, prefix string) (*PostgresStore, <-chan struct{}, func()) {
	test.Helper()
	pause := &graphServingSizePause{prefix: prefix, reached: make(chan struct{}), release: make(chan struct{})}
	peer := graphServingPausedPeerPG(test, ctx, storage, pause)
	var once sync.Once
	release := func() { once.Do(func() { close(pause.release) }) }
	test.Cleanup(release)
	return peer, pause.reached, release
}

func memoryQualificationWaitPG(test *testing.T, ctx context.Context, reached <-chan struct{}, done <-chan error) {
	test.Helper()
	select {
	case <-reached:
	case err := <-done:
		test.Fatal("qualification skipped barrier", err)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
}

func memoryQualificationBlockedPG(test *testing.T, ctx context.Context, storage *PostgresStore, process uint32, done <-chan error) {
	test.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := storage.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1::int))>0`, int64(process)).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			test.Fatal("writer crossed qualification fence", err)
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPGMemoryQualificationAvoidsWriterLockInversion(test *testing.T) {
	for _, mode := range []string{"blob first", "grant first"} {
		test.Run(mode, func(test *testing.T) {
			storage, parent, qualification, _ := memoryQualificationPG(test)
			ctx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			repo, hash := qualification.Source.Graph.RepoID, qualification.Witnesses[0].Hash
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "SELECT epoch::text,head_seq FROM repository_memory_source_state")
			defer release()
			done := make(chan error, 1)
			go func() { _, err := peer.QualifyMemoryRead(ctx, qualification); done <- err }()
			memoryQualificationWaitPG(test, ctx, reached, done)
			writer := catalogTxPG(test, ctx, storage)
			if mode == "blob first" {
				catalogExecPG(test, ctx, writer, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			}
			written := make(chan error, 1)
			go func() {
				_, err := writer.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(hash))
				written <- err
			}()
			memoryQualificationBlockedPG(test, ctx, storage, writer.Conn().PgConn().PID(), written)
			release()
			if err := <-done; !errors.Is(err, domain.ErrConflict) {
				test.Fatal("lock inversion was not a retryable conflict", err)
			}
			if err := <-written; err != nil {
				test.Fatal("qualification did not release source fences", err)
			}
			if memoryQualificationCountPG(test, storage, ctx, repo) != 0 {
				test.Fatal("conflicted attempt left a receipt")
			}
			if err := writer.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.QualifyMemoryRead(ctx, qualification); err != nil {
				test.Fatal("retry after writer rollback", err)
			}
		})
	}
}

func TestPGMemoryQualificationPinsObjectsUntilCommit(test *testing.T) {
	for _, mode := range []string{"rewrite", "revoke"} {
		test.Run(mode, func(test *testing.T) {
			storage, parent, qualification, _ := memoryQualificationPG(test)
			ctx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			repo, hash := qualification.Source.Graph.RepoID, qualification.Witnesses[0].Hash
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "INSERT INTO memory_read_qualifications")
			defer release()
			done := make(chan error, 1)
			var identifier domain.ContentHash
			go func() {
				var err error
				identifier, err = peer.QualifyMemoryRead(ctx, qualification)
				done <- err
			}()
			memoryQualificationWaitPG(test, ctx, reached, done)
			writer := catalogTxPG(test, ctx, storage)
			written := make(chan error, 1)
			go func() {
				var err error
				if mode == "rewrite" {
					_, err = writer.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
				} else {
					_, err = writer.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(hash))
				}
				written <- err
			}()
			memoryQualificationBlockedPG(test, ctx, storage, writer.Conn().PgConn().PID(), written)
			release()
			if err := <-done; err != nil {
				test.Fatal("fenced publication", err)
			}
			if err := <-written; err != nil {
				test.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			if memoryQualificationCountPG(test, storage, ctx, repo) != 1 {
				test.Fatal("concurrent writer destroyed historical receipt")
			}
			called := false
			err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
				called = true
				return nil
			})
			if !errors.Is(err, domain.ErrConflict) || called {
				test.Fatal("post-commit mutation reused old receipt", err)
			}
		})
	}
}

func TestPGMemoryQualificationContentionAndReadAuthorizationScope(test *testing.T) {
	storage, parent, qualification, _ := memoryQualificationPG(test)
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	repo := qualification.Source.Graph.RepoID
	writer := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, writer, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-memory-capture:' || $1,0))`, string(repo))
	if id, err := storage.QualifyMemoryRead(ctx, qualification); !errors.Is(err, domain.ErrConflict) || id != "" {
		test.Fatal("publication ignored in-flight retention work", err)
	}
	if err := writer.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if memoryQualificationCountPG(test, storage, ctx, repo) != 0 {
		test.Fatal("conflicted publication leaked a receipt")
	}
	identifier, err := storage.QualifyMemoryRead(ctx, qualification)
	if err != nil {
		test.Fatal(err)
	}
	for _, authorize := range []func(context.Context) (context.Context, error){nil,
		func(context.Context) (context.Context, error) { return nil, nil },
		func(context.Context) (context.Context, error) { return context.Background(), nil },
		func(context.Context) (context.Context, error) { return nil, domain.ErrForbidden },
	} {
		called := false
		err := storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, authorize, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
			called = true
			return nil
		})
		if err == nil || called {
			test.Fatal("invalid authorization scope exposed a read", err)
		}
	}
	type marker struct{}
	err = storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, func(bound context.Context) (context.Context, error) {
		if !storage.InReadOnlyTransaction(bound) {
			return bound, domain.ErrConflict
		}
		return context.WithValue(bound, marker{}, true), nil
	}, func(bound context.Context, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
		if bound.Value(marker{}) != true {
			return domain.ErrForbidden
		}
		_, err := chunks.ReadChunk(bound, 0)
		return err
	})
	if err != nil {
		test.Fatal("authorizer's returned scope lost", err)
	}
}
