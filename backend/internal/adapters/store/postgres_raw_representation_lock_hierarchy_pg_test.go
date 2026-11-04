//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rawRepresentationHierarchyBarrierKeyPG struct{}

type rawRepresentationHierarchyBarrierPG struct {
	retry    bool
	document domain.ContentHash
	attempts int
	paused   bool
	ready    chan struct{}
	release  chan struct{}
}

func (barrier *rawRepresentationHierarchyBarrierPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(query.SQL)), " ")
	if barrier.retry && strings.HasPrefix(statement, "savepoint ") {
		barrier.attempts++
	}
	match := barrier.retry && strings.HasPrefix(statement, "rollback to savepoint ")
	if !barrier.retry && strings.Contains(statement, "from blobs where hash=$1 for update") && len(query.Args) == 1 {
		match = fmt.Sprint(query.Args[0]) == string(barrier.document)
	}
	if match && !barrier.paused {
		return context.WithValue(ctx, rawRepresentationHierarchyBarrierKeyPG{}, barrier)
	}
	return ctx
}

func (barrier *rawRepresentationHierarchyBarrierPG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	if ctx.Value(rawRepresentationHierarchyBarrierKeyPG{}) != barrier || query.Err != nil || barrier.paused {
		return
	}
	barrier.paused = true
	close(barrier.ready)
	select {
	case <-barrier.release:
	case <-ctx.Done():
	}
}

func TestPGRawRepresentationLockHierarchy(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	for _, order := range []string{"public-doc-first", "raw-grant-before-public-doc"} {
		test.Run(order, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			root, document := rootWithCanonicalDocumentChunk(test)
			canonical := root.bodies[document.Hash]
			plan, chunked := domain.PlanDocChunks(canonical)
			if !chunked || len(plan.Order) != 1 || len(plan.Bodies) != 1 || domain.HashContent(canonical) != document.Hash {
				test.Fatal("lock hierarchy requires a valid manifested document with one actual component")
			}
			component := plan.Order[0]
			if component >= document.Hash || domain.HashContent(plan.Bodies[component]) != component {
				test.Fatal("the real component C must sort before document D without manufactured hashes", component, document.Hash)
			}
			rootRepo, _ := rootPGRepo(test, ctx, storage)
			docRepo, _ := rootPGRepo(test, ctx, storage)
			otherDocRepo, _ := rootPGRepo(test, ctx, storage)
			if err := storage.RequireDocumentIdentity(ctx, rootRepo, domain.DocumentIdentityRootV1); err != nil {
				test.Fatal(err)
			}
			for _, repo := range []domain.ContentHash{docRepo, otherDocRepo} {
				if _, err := storage.PutDoc(ctx, repo, document); err != nil {
					test.Fatal("seed the valid public document and its existing owners", err)
				}
				assertBlobRepresentationDocumentPG(test, storage, ctx, repo, document, canonical, plan, true)
			}
			var payload []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, document.Hash).Scan(&payload); err != nil {
				test.Fatal(err)
			}
			decoded, err := docDecompress(payload)
			manifest, manifested := domain.ParseDocChunkManifest(decoded)
			if err != nil || !manifested || !sameDocManifest(manifest, plan.Manifest) {
				test.Fatal("the first shared attempt must encounter the current valid manifest D", manifested, err)
			}
			remaining := make(map[domain.ContentHash][]byte)
			for hash, body := range root.bodies {
				if hash != document.Hash && hash != component {
					remaining[hash] = body
				}
			}
			if stored, deduped, err := storage.PutChunks(ctx, rootRepo, remaining); err != nil || stored != len(remaining) || deduped != 0 {
				test.Fatal("seed only the unrelated root dependencies before either concurrent producer", stored, deduped, err)
			}
			rawBarrier := &rawRepresentationHierarchyBarrierPG{
				retry: true, ready: make(chan struct{}), release: make(chan struct{}, 1),
			}
			docBarrier := &rawRepresentationHierarchyBarrierPG{
				document: document.Hash, ready: make(chan struct{}), release: make(chan struct{}, 1),
			}
			defer close(rawBarrier.release)
			defer close(docBarrier.release)
			var docTrace pgx.QueryTracer
			if order == "public-doc-first" {
				docTrace = docBarrier
			}
			rawWriter := authorityScopePeerPG(test, storage, "", rawBarrier)
			docWriter := authorityScopePeerPG(test, storage, "", docTrace)
			rawPID := authorityScopePIDPG(test, ctx, rawWriter)
			docPID := authorityScopePIDPG(test, ctx, docWriter)
			if rawPID == docPID || rawPID == fixture.observer.PgConn().PID() || docPID == fixture.observer.PgConn().PID() {
				test.Fatal("lock hierarchy requires two independent producer backends and the existing observer")
			}
			rawBodies := map[domain.ContentHash][]byte{component: plan.Bodies[component], document.Hash: canonical}
			rawStaged, rawCommit := make(chan struct{}), make(chan struct{}, 1)
			defer close(rawCommit)
			rawContext, _, rawDone := authorityScopeRunPG(test, ctx, func(work context.Context) error {
				return rawWriter.WithinRepository(work, rootRepo, func(bound context.Context) error {
					var retained []byte
					if err := rawWriter.db(bound).QueryRow(bound, `SELECT bytes FROM blobs WHERE hash=$1 FOR SHARE`, component).Scan(&retained); err != nil {
						return err
					}
					body, err := docDecompress(retained)
					if err != nil || !bytes.Equal(body, plan.Bodies[component]) || domain.HashContent(body) != component {
						return fmt.Errorf("outer shared retention did not preserve actual component C: %v", err)
					}
					stored, deduped, err := rawWriter.PutChunks(bound, rootRepo, rawBodies)
					if err != nil {
						return err
					}
					if stored != 2 || deduped != 0 {
						return fmt.Errorf("single raw batch lost its original ownership counts: stored=%d deduped=%d", stored, deduped)
					}
					close(rawStaged)
					select {
					case <-rawCommit:
						return nil
					case <-bound.Done():
						return bound.Err()
					}
				})
			})
			authorityScopeReadyPG(test, rawContext, rawBarrier.ready, rawDone)
			if order != "public-doc-first" {
				rawBarrier.release <- struct{}{}
				authorityScopeReadyPG(test, rawContext, rawStaged, rawDone)
			}
			docContext, _, docDone := authorityScopeRunPG(test, ctx, func(work context.Context) error {
				created, err := docWriter.PutDoc(work, docRepo, document)
				if err != nil {
					return err
				}
				if created {
					return errors.New("concurrent PutDoc recreated the existing valid document D")
				}
				return nil
			})
			if order == "public-doc-first" {
				authorityScopeReadyPG(test, docContext, docBarrier.ready, docDone)
				rawBarrier.release <- struct{}{}
				authorityScopeWaitPG(test, rawContext, fixture.observer, docPID, rawPID, "pg_advisory_xact_lock", rawDone)
				docBarrier.release <- struct{}{}
				authorityScopeDonePG(test, docContext, docDone)
				authorityScopeReadyPG(test, rawContext, rawStaged, rawDone)
			} else {
				authorityScopeWaitPG(test, docContext, fixture.observer, rawPID, docPID, "pg_advisory_xact_lock", docDone)
			}
			rawCommit <- struct{}{}
			authorityScopeDonePG(test, rawContext, rawDone)
			if order != "public-doc-first" {
				authorityScopeDonePG(test, docContext, docDone)
			}
			if rawBarrier.attempts != 2 {
				test.Fatal("manifest promotion must finish in the original shared attempt and one fallback, without caller retries", rawBarrier.attempts)
			}
			expected := make(map[domain.ContentHash][]byte, len(root.bodies)+1)
			for hash, body := range root.bodies {
				expected[hash] = body
			}
			expected[component] = plan.Bodies[component]
			hashes := make([]domain.ContentHash, 0, len(expected))
			for hash, body := range expected {
				hashes = append(hashes, hash)
				actual, err := storage.GetChunk(ctx, rootRepo, hash)
				if err != nil || domain.HashContent(actual) != hash || !bytes.Equal(actual, body) {
					test.Fatal("concurrent producers changed an owned raw body's exact bytes or hash", hash, err)
				}
				actual, err = storage.ReadConversationChunk(ctx, rootRepo, hash, int64(len(body)))
				if err != nil || !bytes.Equal(actual, body) {
					test.Fatal("retained raw body is not readable as its bounded root dependency", hash, err)
				}
			}
			owned, err := storage.HasChunks(ctx, rootRepo, hashes)
			if err != nil || len(owned) != len(expected) {
				test.Fatal("the one raw batch did not retain every intended ownership grant", owned, err)
			}
			if _, err := storage.GetDoc(ctx, rootRepo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("raw ownership must not grant the foreign legacy document", err)
			}
			for _, repo := range []domain.ContentHash{docRepo, otherDocRepo} {
				assertBlobRepresentationDocumentPG(test, storage, ctx, repo, document, canonical, plan, true)
			}
			componentSource := readRootSourceWitnessStatePG(test, storage, ctx, component)
			documentSource := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
			job := rootPublicationJob(test, rootRepo, root)
			if err := publishBlobRepresentationRootPG(ctx, storage, job); err != nil {
				test.Fatal("the raw batch must leave a publishable root without repair or producer retries", err)
			}
			receipt, err := storage.GetDocJob(ctx, rootRepo, job.ID)
			if err != nil {
				test.Fatal("read the completed root publication receipt", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, receipt, root)
			assertRootSourceWitnessStatePG(test, storage, ctx, component, componentSource)
			assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, documentSource)
			for _, repo := range []domain.ContentHash{docRepo, otherDocRepo} {
				assertBlobRepresentationDocumentPG(test, storage, ctx, repo, document, canonical, plan, true)
			}
			var unexpected int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1
 AND NOT ((kind='doc' AND hash=$2) OR (kind='chunk' AND hash=ANY($3::text[])))`, rootRepo, root.hash, strs(hashes)).Scan(&unexpected); err != nil || unexpected != 0 {
				test.Fatal("the concurrent producers or root publication leaked unrelated ownership", unexpected, err)
			}
			rootProjectionNoIndex(test, storage, ctx, root.hash)
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}
