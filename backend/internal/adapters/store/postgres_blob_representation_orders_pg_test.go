//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func representationOrdersBlobImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, hashes []domain.ContentHash) string {
	test.Helper()
	var image string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT jsonb_build_object(
 'blobs',(SELECT jsonb_agg(jsonb_build_array(hash,encode(bytes,'hex'),doc_range_source_version,xmin::text) ORDER BY hash)
 FROM blobs WHERE hash=ANY($1)),
 'sources',(SELECT jsonb_agg(jsonb_build_array(hash,incarnation::text,xmin::text) ORDER BY hash)
 FROM blob_source_incarnations WHERE hash=ANY($1)),
 'owners',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,hash,xmin::text) ORDER BY repo_id,kind,hash)
 FROM repo_blobs WHERE hash=ANY($1)))::text`, strs(hashes)).Scan(&image); err != nil {
		test.Fatal("read representation and ownership image", err)
	}
	return image
}

func assertRepresentationOrdersManifestPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, document domain.SessionDoc, manifest domain.DocChunkManifest) {
	test.Helper()
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	plan, chunked := domain.PlanDocChunks(canonical)
	if !chunked || !sameDocManifest(manifest, plan.Manifest) {
		test.Fatal("manifest no longer describes the canonical document", manifest)
	}
	components := make([][]byte, 0, len(manifest.Chunks))
	for _, hash := range manifest.Chunks {
		body, err := storage.GetChunk(ctx, repo, hash)
		if err != nil || !bytes.Equal(body, plan.Bodies[hash]) {
			test.Fatal("document owner lost a current manifest component", repo, hash, err)
		}
		components = append(components, body)
	}
	assembled, err := domain.AssembleDocChunks(manifest, components, document.Hash)
	if err != nil || !bytes.Equal(assembled, canonical) {
		test.Fatal("returned manifest cannot reconstruct the document", repo, err)
	}
	actual, err := storage.GetDoc(ctx, repo, document.Hash)
	if err != nil {
		test.Fatal("document is no longer readable", repo, err)
	}
	if err := domain.ValidateSessionDocHash(actual); err != nil || actual.Hash != document.Hash {
		test.Fatal("document identity changed", repo, err)
	}
	actualCanonical, err := domain.CanonicalBytes(actual.CIR)
	if err != nil || !bytes.Equal(actualCanonical, canonical) {
		test.Fatal("document content changed", repo, err)
	}
}

func assertRepresentationOrdersReplaysPG(test *testing.T, storage *PostgresStore, ctx context.Context, owners []domain.ContentHash, document domain.SessionDoc, source rootSourceWitnessStatePG) {
	test.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		for _, owner := range owners {
			if _, err := storage.PutDoc(ctx, owner, document); err != nil {
				test.Fatal("repeated public document write rejected a pinned raw representation", owner, attempt, err)
			}
			assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, source)
			manifest, err := storage.GetDocManifest(ctx, owner, document.Hash)
			if err != nil {
				test.Fatal("repeated public manifest read rejected a pinned raw representation", owner, attempt, err)
			}
			assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, source)
			assertRepresentationOrdersManifestPG(test, storage, ctx, owner, document, manifest)
		}
	}
}

func TestBlobRepresentationOrdersPGDocumentBeforeRoot(test *testing.T) {
	for _, scope := range []string{"same-repo", "cross-repo"} {
		test.Run(scope+"/"+time.Now().UTC().Format(time.RFC3339Nano), func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			fixture, document := rootWithCanonicalDocumentChunk(test)
			documentRepo, _ := rootPGRepo(test, ctx, storage)
			if _, err := storage.PutDoc(ctx, documentRepo, document); err != nil {
				test.Fatal("publish document before its canonical bytes are needed as a root chunk", err)
			}
			before := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
			stored, err := docDecompress(before.body)
			if err != nil {
				test.Fatal(err)
			}
			manifest, recognized := domain.ParseDocChunkManifest(stored)
			if !recognized {
				test.Fatal("document-first setup did not store an actual legacy manifest")
			}
			assertRepresentationOrdersManifestPG(test, storage, ctx, documentRepo, document, manifest)
			rootRepo := documentRepo
			if scope == "cross-repo" {
				rootRepo, _ = rootPGRepo(test, ctx, storage)
			}
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, rootRepo)
			source := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
			raw, err := docDecompress(source.body)
			if err != nil || !bytes.Equal(raw, fixture.bodies[document.Hash]) {
				test.Fatal("raw grant did not recover the existing document's exact canonical bytes", err)
			}
			if source.version <= before.version || source.incarnation != before.incarnation {
				test.Fatal("manifest-to-raw conversion did not advance the existing source", before, source)
			}
			chunk, err := storage.ReadConversationChunk(ctx, rootRepo, document.Hash, domain.ConversationManifestChunkBytes)
			if err != nil || !bytes.Equal(chunk, fixture.bodies[document.Hash]) {
				test.Fatal("root owner cannot read the converted dependency", err)
			}
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			owners := []domain.ContentHash{documentRepo}
			assertRepresentationOrdersReplaysPG(test, storage, ctx, owners, document, source)
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				test.Fatal("document replay invalidated the prepared root without any source repair", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			assertRepresentationOrdersReplaysPG(test, storage, ctx, owners, document, source)
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
		})
	}
}

func TestBlobRepresentationOrdersPGLazyManifestAfterRootPin(test *testing.T) {
	for _, scope := range []string{"same-repo", "cross-repo"} {
		test.Run(scope+"/"+time.Now().UTC().Format(time.RFC3339Nano), func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			fixture, document := rootWithCanonicalDocumentChunk(test)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, "")
			documentRepo := job.RepoID
			if scope == "cross-repo" {
				documentRepo, _ = rootPGRepo(test, ctx, storage)
			}
			peerRepo, _ := rootPGRepo(test, ctx, storage)
			owners := []domain.ContentHash{documentRepo, peerRepo}
			for _, owner := range owners {
				if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, owner, document.Hash); err != nil {
					test.Fatal("seed a legacy document grant over an existing valid raw root chunk", err)
				}
			}
			plan, chunked := domain.PlanDocChunks(fixture.bodies[document.Hash])
			if !chunked {
				test.Fatal("nested legacy document has no manifest plan")
			}
			for _, hash := range plan.Order {
				if _, err := storage.GetChunk(ctx, peerRepo, hash); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("legacy fixture already granted a planned component", hash, err)
				}
			}
			source := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
			raw, err := docDecompress(source.body)
			if err != nil || !bytes.Equal(raw, fixture.bodies[document.Hash]) {
				test.Fatal("lazy-repack setup is not the canonical raw document", err)
			}
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			manifest, err := storage.GetDocManifest(ctx, documentRepo, document.Hash)
			if err != nil {
				test.Fatal("public lazy manifest preparation failed for a pinned legacy document", err)
			}
			assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, source)
			for _, owner := range owners {
				assertRepresentationOrdersManifestPG(test, storage, ctx, owner, document, manifest)
			}
			assertRepresentationOrdersReplaysPG(test, storage, ctx, owners, document, source)
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				test.Fatal("public lazy repack invalidated an already prepared root", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			assertRepresentationOrdersReplaysPG(test, storage, ctx, owners, document, source)
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
		})
	}
}

func TestBlobRepresentationOrdersPGRawGrantRejectsBrokenExistingManifest(test *testing.T) {
	for _, scope := range []string{"same-repo", "cross-repo"} {
		for _, damage := range []string{"missing-component", "current-corrupt-component"} {
			test.Run(scope+"/"+damage, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := uniqueRootPublicationFixturePG(test, "broken-manifest:"+test.Name())
				document := domain.SessionDoc{Hash: domain.HashContent(fixture.canonical), CIR: fixture.cir}
				plan, chunked := domain.PlanDocChunks(fixture.canonical)
				if !chunked || len(plan.Order) == 0 {
					test.Fatal("broken-manifest fixture has no components")
				}
				documentRepo, documentNamespace := rootPGRepo(test, ctx, storage)
				if _, err := storage.PutDoc(ctx, documentRepo, document); err != nil {
					test.Fatal("publish the existing manifest through the public API", err)
				}
				manifest, err := storage.GetDocManifest(ctx, documentRepo, document.Hash)
				if err != nil {
					test.Fatal(err)
				}
				assertRepresentationOrdersManifestPG(test, storage, ctx, documentRepo, document, manifest)
				rawRepo, rawNamespace := documentRepo, documentNamespace
				if scope == "cross-repo" {
					rawRepo, rawNamespace = rootPGRepo(test, ctx, storage)
				}
				component := plan.Order[0]
				if damage == "missing-component" {
					if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE hash=$1`, component); err != nil {
						test.Fatal(err)
					}
					if result, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, component); err != nil || result.RowsAffected() != 1 {
						test.Fatal("remove the current existing-manifest component", err)
					}
				} else {
					corrupt := bytes.Clone(plan.Bodies[component])
					corrupt[len(corrupt)-1] ^= 1
					if result, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, component, docCompress(corrupt)); err != nil || result.RowsAffected() != 1 {
						test.Fatal("corrupt the current component without changing its length", err)
					}
				}
				probeBody := []byte("unrelated batch member:" + string(document.Hash))
				probeHash := domain.HashContent(probeBody)
				bodies := map[domain.ContentHash][]byte{document.Hash: fixture.canonical, probeHash: probeBody}
				affected := append([]domain.ContentHash{document.Hash, probeHash}, plan.Order...)
				before := representationOrdersBlobImagePG(test, storage, ctx, affected)
				documentBefore := rootPGImage(test, ctx, storage, documentRepo, documentNamespace)
				rawBefore := rootPGImage(test, ctx, storage, rawRepo, rawNamespace)
				stored, deduped, err := storage.PutChunks(ctx, rawRepo, bodies)
				if !errors.Is(err, domain.ErrIntegrity) || stored != 0 || deduped != 0 {
					test.Fatal("raw grant trusted incoming canonical bytes instead of validating the existing manifest", stored, deduped, err)
				}
				if after := representationOrdersBlobImagePG(test, storage, ctx, affected); after != before {
					test.Fatal("failed raw grant reconstructed a component, replaced a source, or leaked partial blobs or ownership")
				}
				if after := rootPGImage(test, ctx, storage, documentRepo, documentNamespace); after != documentBefore {
					test.Fatal("failed raw grant changed the existing document owner's state")
				}
				if after := rootPGImage(test, ctx, storage, rawRepo, rawNamespace); after != rawBefore {
					test.Fatal("failed raw grant leaked ownership, accounting, or publication state")
				}
				owned, err := storage.HasChunks(ctx, rawRepo, []domain.ContentHash{document.Hash, probeHash})
				if err != nil || len(owned) != 0 {
					test.Fatal("failed batch exposed a raw grant", owned, err)
				}
				if _, err := storage.GetChunk(ctx, rawRepo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("failed conversion exposed document bytes as a raw chunk", err)
				}
				if _, err := storage.GetDoc(ctx, documentRepo, document.Hash); err == nil {
					test.Fatal("broken existing manifest was silently repaired from the incoming raw body")
				}
			})
		}
	}
}
