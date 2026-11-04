//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func blobRepresentationImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, hashes []domain.ContentHash) string {
	test.Helper()
	var image string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(
 source.hash,encode(source.bytes,'hex'),source.doc_range_source_version,source.xmin::text,incarnation.incarnation::text)
 ORDER BY source.hash),'[]'::jsonb)::text FROM blobs source
 LEFT JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=ANY($1::text[])`, strs(hashes)).Scan(&image); err != nil {
		test.Fatal("read physical representation rollback image", err)
	}
	return image
}

func assertBlobRepresentationDocumentPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, document domain.SessionDoc, canonical []byte, plan domain.DocChunkPlan, requireChunks bool) {
	test.Helper()
	actual, err := storage.GetDoc(ctx, repo, document.Hash)
	if err != nil {
		test.Fatal("conversion or raw ownership lost a legacy document reader", err)
	}
	actualBytes, err := domain.CanonicalBytes(actual.CIR)
	if err != nil || actual.Hash != document.Hash || domain.HashContent(actualBytes) != document.Hash || !bytes.Equal(actualBytes, canonical) {
		test.Fatal("legacy document reader changed its canonical bytes or logical hash", err)
	}
	owned, err := storage.HasDocs(ctx, repo, []domain.ContentHash{document.Hash})
	if err != nil || !slices.Equal(owned, []domain.ContentHash{document.Hash}) {
		test.Fatal("conversion lost an existing document grant", owned, err)
	}
	chunks, err := storage.HasChunks(ctx, repo, plan.Order)
	if err != nil || requireChunks && len(chunks) != len(plan.Bodies) {
		test.Fatal("manifest conversion did not retain its readable component grants", chunks, err)
	}
	for _, hash := range chunks {
		body, err := storage.GetChunk(ctx, repo, hash)
		if err != nil || domain.HashContent(body) != hash || !bytes.Equal(body, plan.Bodies[hash]) {
			test.Fatal("document component grant lost its current verified body", hash, err)
		}
	}
	var unexpected int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1
 AND NOT ((kind='doc' AND hash=$2) OR (kind='chunk' AND hash=ANY($3::text[])))`, repo, document.Hash, strs(plan.Order)).Scan(&unexpected); err != nil || unexpected != 0 {
		test.Fatal("conversion granted an unrelated document or raw object", unexpected, err)
	}
}

func assertBlobRepresentationRootAbsentPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, fixture rootReaderFixture) {
	test.Helper()
	var grants, jobs, roots int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1),
 (SELECT count(*) FROM blobs WHERE hash=$2)`, repo, fixture.hash).Scan(&grants, &jobs, &roots); err != nil || grants != 0 || jobs != 0 || roots != 0 {
		test.Fatal("uncommitted or rejected raw grant leaked root ownership, body, or receipt", grants, jobs, roots, err)
	}
	if counts := rangeCountsPG(test, storage, ctx, fixture.hash); counts != [2]int{} {
		test.Fatal("uncommitted or rejected raw grant leaked a root range index", counts)
	}
	rootProjectionNoIndex(test, storage, ctx, fixture.hash)
}

func publishBlobRepresentationRootPG(ctx context.Context, storage *PostgresStore, job domain.DocFinalizationJob) (result error) {
	if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
		return err
	}
	claimed, err := storage.ClaimDocJob(ctx, job.RepoID, time.Now().UTC(), time.Minute)
	if err != nil {
		return err
	}
	publication, err := storage.PrepareRootDocJob(ctx, claimed)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, publication.Close()) }()
	if err := publication.Complete(ctx, claimed, time.Now()); err != nil {
		return err
	}
	receipt, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || receipt.State != "completed" || receipt.DocumentRef() != claimed.DocumentRef() || receipt.Version != claimed.Version || !receipt.LeaseUntil.IsZero() {
		return fmt.Errorf("borrowed raw grant did not stage the completed root receipt: receipt=%+v err=%v", receipt, err)
	}
	var headers, pages int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$1),
 (SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$1)`, job.DocHash).Scan(&headers, &pages); err != nil {
		return err
	}
	if headers != 1 || pages == 0 {
		return fmt.Errorf("borrowed raw grant did not stage the complete root range index: headers=%d pages=%d", headers, pages)
	}
	return nil
}

func blobRepresentationSchedulePG(test *testing.T, storage *PostgresStore, ctx context.Context, observer *pgx.Conn, conversion, order, rejection string) {
	test.Helper()
	directory := rangeSpoolTempDir(test)
	fixture, document := rootWithCanonicalDocumentChunk(test)
	canonical := fixture.bodies[document.Hash]
	plan, chunked := domain.PlanDocChunks(canonical)
	if !chunked || len(canonical) != domain.ConversationManifestChunkBytes || domain.HashContent(canonical) != document.Hash || len(fixture.bodies) < 2 || len(plan.Order) != 1 || plan.Order[0] == document.Hash {
		test.Fatal("schedule requires the original bounded, valid cross-kind canonical overlap")
	}
	rootRepo, rootNamespace := rootPGRepo(test, ctx, storage)
	docRepo, docNamespace := rootPGRepo(test, ctx, storage)
	otherDocRepo, otherDocNamespace := rootPGRepo(test, ctx, storage)
	if err := storage.RequireDocumentIdentity(ctx, rootRepo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, document.Hash, docCompress(canonical)); err != nil {
		test.Fatal("seed a valid legacy document representation before either producer", err)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$3),($2,'doc',$3)`, docRepo, otherDocRepo, document.Hash); err != nil {
		test.Fatal("seed the two existing legacy document readers", err)
	}
	initial := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
	assertBlobRepresentationDocumentPG(test, storage, ctx, docRepo, document, canonical, plan, false)
	assertBlobRepresentationDocumentPG(test, storage, ctx, otherDocRepo, document, canonical, plan, false)
	assertBlobRepresentationRootAbsentPG(test, storage, ctx, rootRepo, fixture)
	job := rootPublicationJob(test, rootRepo, fixture)
	extra := []byte("uncommitted extra raw body:" + test.Name())
	extraHash := domain.HashContent(extra)
	invalid := []byte("invalid raw body:" + test.Name())
	invalidHash := domain.HashContent([]byte("different raw body:" + test.Name()))
	hashes := []domain.ContentHash{fixture.hash, document.Hash, extraHash, invalidHash}
	for hash := range fixture.bodies {
		hashes = append(hashes, hash)
	}
	hashes = append(hashes, plan.Order...)
	rawWriter := authorityScopePeerPG(test, storage, "", nil)
	converter := authorityScopePeerPG(test, storage, "", nil)
	rawPID := authorityScopePIDPG(test, ctx, rawWriter)
	converterPID := authorityScopePIDPG(test, ctx, converter)
	if rawPID == converterPID || rawPID == observer.PgConn().PID() || converterPID == observer.PgConn().PID() {
		test.Fatal("representation schedule requires independent raw, converter, and observer PostgreSQL backends")
	}
	convert := func(bound context.Context) error {
		if conversion == "PutDoc" {
			created, err := converter.PutDoc(bound, docRepo, document)
			if err != nil {
				return err
			}
			if created {
				return errors.New("PutDoc reported the existing canonical document as newly created")
			}
			return nil
		}
		manifest, err := converter.GetDocManifest(bound, docRepo, document.Hash)
		if err != nil {
			return err
		}
		if !sameDocManifest(manifest, plan.Manifest) {
			return errors.New("GetDocManifest changed the valid canonical document's chunk plan")
		}
		return nil
	}
	grant := func(bound context.Context) error {
		stored, deduped, err := rawWriter.PutChunks(bound, rootRepo, fixture.bodies)
		if err != nil {
			return err
		}
		if stored != len(fixture.bodies) || deduped != 0 {
			return fmt.Errorf("automatic raw grant returned incorrect ownership counts: stored=%d deduped=%d", stored, deduped)
		}
		return nil
	}
	holder, holderRepo, holderPID, waiterPID, waitStatement := converter, docRepo, converterPID, rawPID, "pg_advisory_xact_lock_shared"
	first, second := convert, grant
	if order == "raw-grant-first" {
		holder, holderRepo, holderPID, waiterPID, waitStatement = rawWriter, rootRepo, rawPID, converterPID, "pg_advisory_xact_lock("
		first, second = grant, convert
	}
	var pending context.Context
	var completed <-chan error
	var expectedSource rootSourceWitnessStatePG
	var expectedImages [4]string
	held, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := holder.WithinRepository(held, holderRepo, func(bound context.Context) error {
		if err := first(bound); err != nil {
			return err
		}
		if order == "raw-grant-first" {
			assertRootSourceWitnessStatePG(test, holder, bound, document.Hash, initial)
			owned, err := holder.HasChunks(bound, rootRepo, conversationChunkOrder(fixture.manifest))
			if err != nil || len(owned) != len(fixture.bodies) {
				return fmt.Errorf("borrowed PutChunks did not stage all root grants: owned=%v err=%v", owned, err)
			}
		} else {
			var stored []byte
			if err := holder.db(bound).QueryRow(bound, `SELECT bytes FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored); err != nil {
				return err
			}
			decoded, err := docDecompress(stored)
			manifest, isManifest := domain.ParseDocChunkManifest(decoded)
			if err != nil || !isManifest || !sameDocManifest(manifest, plan.Manifest) {
				return fmt.Errorf("converter-first producer did not stage a real manifest: manifest=%t err=%v", isManifest, err)
			}
			if rejection == "invalid-component" {
				componentHash := plan.Order[0]
				damaged := bytes.Clone(plan.Bodies[componentHash])
				damaged[len(damaged)-1] ^= 1
				result, err := holder.db(bound).Exec(bound, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, componentHash, docCompress(damaged))
				if err != nil || result.RowsAffected() != 1 {
					return fmt.Errorf("could not isolate one invalid current manifest component: err=%v", err)
				}
			}
			expectedSource = readRootSourceWitnessStatePG(test, holder, bound, document.Hash)
			expectedImages = [4]string{
				rootPGImage(test, bound, holder, rootRepo, rootNamespace),
				rootPGImage(test, bound, holder, docRepo, docNamespace),
				rootPGImage(test, bound, holder, otherDocRepo, otherDocNamespace),
				blobRepresentationImagePG(test, holder, bound, hashes),
			}
		}
		assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
		assertBlobRepresentationRootAbsentPG(test, storage, ctx, rootRepo, fixture)
		pending, _, completed = authorityScopeRunPG(test, ctx, func(work context.Context) error {
			waiter, waiterRepo := rawWriter, rootRepo
			if order == "raw-grant-first" {
				waiter, waiterRepo = converter, docRepo
			}
			rejected := false
			err := waiter.WithinRepository(work, waiterRepo, func(write context.Context) error {
				if err := second(write); err != nil {
					if rejection == "invalid-component" && errors.Is(err, domain.ErrIntegrity) {
						rejected = true
					}
					return err
				}
				if rejection == "invalid-component" {
					return errors.New("raw producer repaired or accepted an invalid current manifest component")
				}
				if rejection != "invalid-batch" {
					return nil
				}
				if err := publishBlobRepresentationRootPG(write, rawWriter, job); err != nil {
					return fmt.Errorf("stage the root and its receipt before the invalid batch: %w", err)
				}
				stored, deduped, err := rawWriter.PutChunks(write, rootRepo, map[domain.ContentHash][]byte{extraHash: extra, invalidHash: invalid})
				if stored != 0 || deduped != 0 || !errors.Is(err, domain.ErrIntegrity) {
					return fmt.Errorf("one invalid raw chunk changed the original error/count contract: stored=%d deduped=%d err=%v", stored, deduped, err)
				}
				rejected = true
				return err
			})
			if rejection != "" {
				if !rejected || !errors.Is(err, domain.ErrIntegrity) {
					return fmt.Errorf("raw producer did not preserve the intended integrity rollback: rejected=%t err=%v", rejected, err)
				}
				return nil
			}
			return err
		})
		authorityScopeWaitPG(test, pending, observer, holderPID, waiterPID, waitStatement, completed)
		assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
		assertBlobRepresentationDocumentPG(test, storage, ctx, docRepo, document, canonical, plan, false)
		assertBlobRepresentationDocumentPG(test, storage, ctx, otherDocRepo, document, canonical, plan, false)
		assertBlobRepresentationRootAbsentPG(test, storage, ctx, rootRepo, fixture)
		return nil
	}); err != nil {
		test.Fatal("first public producer failed its retained outer transaction", err)
	}
	authorityScopeDonePG(test, pending, completed)
	if rejection != "" {
		actualImages := [4]string{
			rootPGImage(test, ctx, storage, rootRepo, rootNamespace),
			rootPGImage(test, ctx, storage, docRepo, docNamespace),
			rootPGImage(test, ctx, storage, otherDocRepo, otherDocNamespace),
			blobRepresentationImagePG(test, storage, ctx, hashes),
		}
		if actualImages != expectedImages {
			test.Fatal("rejected raw producer leaked promotion, bytes, grants, accounting, indexes, or a root receipt")
		}
		assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, expectedSource)
		assertBlobRepresentationRootAbsentPG(test, storage, ctx, rootRepo, fixture)
		if rejection == "invalid-component" {
			for _, repo := range []domain.ContentHash{docRepo, otherDocRepo} {
				if _, err := storage.GetDoc(ctx, repo, document.Hash); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("invalid current manifest component was repaired from the caller's valid raw body", err)
				}
			}
		} else {
			assertBlobRepresentationDocumentPG(test, storage, ctx, docRepo, document, canonical, plan, true)
			assertBlobRepresentationDocumentPG(test, storage, ctx, otherDocRepo, document, canonical, plan, true)
		}
		assertRangeSpoolsRemoved(test, directory)
		return
	}
	var stored []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored); err != nil {
		test.Fatal(err)
	}
	decoded, err := docDecompress(stored)
	if err != nil || !bytes.Equal(decoded, canonical) || domain.HashContent(decoded) != document.Hash {
		test.Fatal("successful public raw producer did not retain or automatically promote the exact valid raw representation", err)
	}
	if order == "raw-grant-first" {
		assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
	} else {
		current := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
		if current.version <= expectedSource.version || current.incarnation != expectedSource.incarnation {
			test.Fatal("converter-first raw grant did not promote the existing manifest source internally", current.version, expectedSource.version)
		}
	}
	assertBlobRepresentationDocumentPG(test, storage, ctx, docRepo, document, canonical, plan, true)
	assertBlobRepresentationDocumentPG(test, storage, ctx, otherDocRepo, document, canonical, plan, false)
	for hash, expected := range fixture.bodies {
		body, err := storage.GetChunk(ctx, rootRepo, hash)
		if err != nil || domain.HashContent(body) != hash || !bytes.Equal(body, expected) {
			test.Fatal("successful raw grant lost an owned body's exact bytes or hash", hash, err)
		}
		body, err = storage.ReadConversationChunk(ctx, rootRepo, hash, int64(len(expected)))
		if err != nil || !bytes.Equal(body, expected) {
			test.Fatal("raw grant is not readable as its original bounded root dependency", hash, err)
		}
	}
	if _, err := storage.GetDoc(ctx, rootRepo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("raw chunk ownership granted a foreign legacy document", err)
	}
	if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal("automatic raw grant did not leave an enqueueable root", err)
	}
	claimed, err := storage.ClaimDocJob(ctx, rootRepo, time.Now().UTC(), time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	before := rootPGImage(test, ctx, storage, rootRepo, rootNamespace)
	publication := prepareRootStreamingPG(test, storage, ctx, claimed, directory)
	assertRootStreamingUnpublishedPG(test, storage, ctx, claimed)
	if after := rootPGImage(test, ctx, storage, rootRepo, rootNamespace); after != before {
		test.Fatal("preparing the automatically retained raw root mutated durable state")
	}
	if err := publication.Complete(ctx, claimed, time.Now()); err != nil {
		test.Fatal("root required a manual raw repair, producer retry, or fresh preparation", err)
	}
	assertRootSourceWitnessPublishedPG(test, storage, ctx, claimed, fixture)
	rootProjectionNoIndex(test, storage, ctx, fixture.hash)
	assertBlobRepresentationDocumentPG(test, storage, ctx, docRepo, document, canonical, plan, true)
	assertBlobRepresentationDocumentPG(test, storage, ctx, otherDocRepo, document, canonical, plan, false)
}

func TestPGBlobRepresentationConcurrentPutChunksAndDocumentConversion(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	for _, conversion := range []string{"PutDoc", "GetDocManifest"} {
		for _, order := range []string{"raw-grant-first", "converter-first"} {
			test.Run(conversion+"/"+order, func(test *testing.T) {
				blobRepresentationSchedulePG(test, storage, ctx, fixture.observer, conversion, order, "")
			})
		}
	}
}

func TestPGBlobRepresentationConcurrentRawGrantIntegrityRollback(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	ctx := rootPublicationContext(fixture.ctx)
	for _, conversion := range []string{"PutDoc", "GetDocManifest"} {
		for _, rejection := range []string{"invalid-batch", "invalid-component"} {
			test.Run(conversion+"/"+rejection, func(test *testing.T) {
				blobRepresentationSchedulePG(test, storage, ctx, fixture.observer, conversion, "converter-first", rejection)
			})
		}
	}
}
