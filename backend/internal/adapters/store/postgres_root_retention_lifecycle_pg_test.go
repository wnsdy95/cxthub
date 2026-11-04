//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rootRetentionDependencyReadKeyPG struct{}

type rootRetentionDependencyReadPausePG struct {
	repo       domain.ContentHash
	dependency domain.ContentHash
	ready      chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (pause *rootRetentionDependencyReadPausePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(query.SQL)), " ")
	if strings.HasPrefix(statement, "select substring(b.bytes from 1 for $3),b.doc_range_source_version,incarnation.incarnation::text ") &&
		strings.Contains(statement, "rb.kind='chunk' and rb.hash=$2") && len(query.Args) == 3 &&
		fmt.Sprint(query.Args[0]) == string(pause.repo) && fmt.Sprint(query.Args[1]) == string(pause.dependency) {
		return context.WithValue(ctx, rootRetentionDependencyReadKeyPG{}, pause)
	}
	return ctx
}

func (pause *rootRetentionDependencyReadPausePG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	if ctx.Value(rootRetentionDependencyReadKeyPG{}) != pause || query.Err != nil || query.CommandTag.RowsAffected() != 1 {
		return
	}
	pause.once.Do(func() {
		close(pause.ready)
		select {
		case <-pause.release:
		case <-ctx.Done():
		}
	})
}

func assertRootRetentionDependenciesPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, fixture rootReaderFixture, sources map[domain.ContentHash]rootSourceWitnessStatePG) {
	test.Helper()
	for hash, expected := range fixture.bodies {
		source, exists := sources[hash]
		if !exists {
			test.Fatal("retention fixture omitted a dependency source", hash)
		}
		assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
		body, err := storage.ReadConversationChunk(ctx, job.RepoID, hash, int64(len(expected)))
		if err != nil || !bytes.Equal(body, expected) || domain.HashContent(body) != hash {
			test.Fatal("sibling lifecycle changed the root's owned raw dependency", hash, err)
		}
	}
}

func assertRootRetentionSiblingDeletedPG(test *testing.T, storage *PostgresStore, ctx context.Context, rootRepo, siblingRepo, hash domain.ContentHash) {
	test.Helper()
	owned, err := storage.HasDocs(ctx, siblingRepo, []domain.ContentHash{hash})
	if err != nil || len(owned) != 0 {
		test.Fatal("public DeleteDoc retained the sibling document grant", owned, err)
	}
	if _, err := storage.GetDoc(ctx, siblingRepo, hash); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("deleted sibling document remains publicly readable", err)
	}
	var otherGrants, snapshots int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM repo_blobs WHERE hash=$1 AND NOT (repo_id=$2 AND kind='chunk')),
 (SELECT count(*) FROM snapshots WHERE doc_hash=$1 OR memory_hash=$1)`, hash, rootRepo).Scan(&otherGrants, &snapshots); err != nil || otherGrants != 0 || snapshots != 0 {
		test.Fatal("retention was masked by another owner or snapshot instead of the root chunk grant", otherGrants, snapshots, err)
	}
}

func rootRetentionLifecyclePG(test *testing.T, duringPreparation bool) {
	test.Helper()
	for _, scope := range []string{"same-repo", "cross-repo"} {
		test.Run(scope+"/"+time.Now().UTC().Format(time.RFC3339Nano), func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			fixture, document := rootWithCanonicalDocumentChunk(test)
			rootRepo, _ := rootPGRepo(test, ctx, storage)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, rootRepo)
			siblingRepo := rootRepo
			if scope == "cross-repo" {
				siblingRepo, _ = rootPGRepo(test, ctx, storage)
			}
			sources := make(map[domain.ContentHash]rootSourceWitnessStatePG, len(fixture.bodies))
			for hash, expected := range fixture.bodies {
				source := readRootSourceWitnessStatePG(test, storage, ctx, hash)
				body, err := docDecompress(source.body)
				if err != nil || !bytes.Equal(body, expected) || domain.HashContent(body) != hash {
					test.Fatal("root grant did not retain the actual canonical raw dependency", hash, err)
				}
				sources[hash] = source
			}
			created, err := storage.PutDoc(ctx, siblingRepo, document)
			if err != nil || created {
				test.Fatal("public sibling PutDoc must reuse the root's existing same-hash raw source", created, err)
			}
			assertRootRetentionDependenciesPG(test, storage, ctx, job, fixture, sources)
			sibling, err := storage.GetDoc(ctx, siblingRepo, document.Hash)
			if err != nil {
				test.Fatal("public sibling PutDoc did not establish a readable document owner", err)
			}
			canonical, err := domain.CanonicalBytes(sibling.CIR)
			if err != nil || sibling.Hash != document.Hash || !bytes.Equal(canonical, fixture.bodies[document.Hash]) {
				test.Fatal("sibling document does not share the exact valid root dependency", err)
			}
			before, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
			if err != nil {
				test.Fatal(err)
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			var publication outbound.PreparedRootDocPublication
			if duringPreparation {
				pause := &rootRetentionDependencyReadPausePG{repo: job.RepoID, dependency: document.Hash,
					ready: make(chan struct{}), release: make(chan struct{})}
				preparer := authorityScopePeerPG(test, storage, "", pause)
				var release sync.Once
				unblock := func() { release.Do(func() { close(pause.release) }) }
				defer unblock()
				work, cancel, completed := authorityScopeRunPG(test, ctx, func(work context.Context) error {
					defer unblock()
					select {
					case <-pause.ready:
					case <-work.Done():
						return work.Err()
					}
					return storage.DeleteDoc(work, siblingRepo, document.Hash)
				})
				defer cancel()
				publication = prepareRootStreamingPG(test, preparer, work, job, directory)
				authorityScopeDonePG(test, work, completed)
			} else {
				publication = prepareRootStreamingPG(test, storage, ctx, job, directory)
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				assertRootRetentionDependenciesPG(test, storage, ctx, job, fixture, sources)
				deletion, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := storage.DeleteDoc(deletion, siblingRepo, document.Hash); err != nil {
					test.Fatal("public DeleteDoc failed in the prepared-before-complete window", err)
				}
			}
			assertRootRetentionSiblingDeletedPG(test, storage, ctx, job.RepoID, siblingRepo, document.Hash)
			assertRootRetentionDependenciesPG(test, storage, ctx, job, fixture, sources)
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
			if err != nil || !reflect.DeepEqual(current, before) {
				test.Fatal("sibling deletion or preparation changed the running root receipt", current, err)
			}
			paths, err := filepath.Glob(filepath.Join(directory, "cxthub-doc-range-*"))
			if err != nil || len(paths) != 2 {
				test.Fatal("valid preparation did not retain its original and derived publication spools", paths, err)
			}
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				test.Fatal("sibling deletion invalidated valid root preparation without source changes", err)
			}
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
			assertRootRetentionSiblingDeletedPG(test, storage, ctx, job.RepoID, siblingRepo, document.Hash)
			assertRootRetentionDependenciesPG(test, storage, ctx, job, fixture, sources)
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			ready, err := storage.HasDocRangeIndex(ctx, job.RepoID, job.DocHash)
			if err != nil || !ready {
				test.Fatal("completed root has no current source-bound range generation", ready, err)
			}
			var receipts int
			if err := storage.db(ctx).QueryRow(ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1`, job.RepoID).Scan(&receipts); err != nil || receipts != 1 {
				test.Fatal("sibling lifecycle did not leave exactly one root publication receipt", receipts, err)
			}
		})
	}
}

func TestRootRetentionLifecyclePGActualDeleteDocDuringPreparation(test *testing.T) {
	rootRetentionLifecyclePG(test, true)
}

func TestRootRetentionLifecyclePGActualDeleteDocPreparedBeforeComplete(test *testing.T) {
	rootRetentionLifecyclePG(test, false)
}
