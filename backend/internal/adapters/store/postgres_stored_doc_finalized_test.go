//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// finalizedDocFixturePG publishes one legacy v2 document through the streaming
// finalization path and returns its completed job.
func finalizedDocFixturePG(t *testing.T, storage *PostgresStore, ctx context.Context) domain.DocFinalizationJob {
	t.Helper()
	job, _, _ := preparedJobFixturePG(t, storage, ctx)
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	if err := publication.Stage(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPGFinalizedDocReferenceNeedsTheCompletedJobOfTheStoredDocument(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	legacy := func(job domain.DocFinalizationJob) domain.DocumentRef {
		return domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}
	}

	t.Run("not published yet", func(t *testing.T) {
		job, _, _ := preparedJobFixturePG(t, storage, ctx)
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); found || err != nil {
			t.Fatalf("running job attested an unpublished document: found=%v err=%v", found, err)
		}
		if _, err := storage.VerifyStoredDoc(ctx, job.RepoID, job.DocHash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("byte verification did not refuse the unpublished document: %v", err)
		}
	})

	t.Run("completed job attests without reading chunk bodies", func(t *testing.T) {
		job := finalizedDocFixturePG(t, storage, ctx)
		proof, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash)
		if err != nil || !found || proof.DocumentRef() != legacy(job) {
			t.Fatalf("completed job not attested: found=%v proof=%+v err=%v", found, proof.DocumentRef(), err)
		}
		// The record stands for the verification that completed the job. Later
		// damage to a stored chunk is found by the byte verification that readers
		// and VerifyStoredDoc still run, not by this check.
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, job.Manifest.Chunks[0], docCompress([]byte(`{"damaged":true}`))); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || !found {
			t.Fatalf("attestation read a chunk body: found=%v err=%v", found, err)
		}
		if _, err := storage.VerifyStoredDoc(ctx, job.RepoID, job.DocHash); err == nil {
			t.Fatal("byte verification accepted a damaged chunk")
		}
	})

	t.Run("a chunk the repository no longer owns", func(t *testing.T) {
		job := finalizedDocFixturePG(t, storage, ctx)
		if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, job.RepoID, job.Manifest.Chunks[0]); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || found {
			t.Fatalf("attested a document with an unowned chunk: found=%v err=%v", found, err)
		}
	})

	t.Run("a job that is no longer completed", func(t *testing.T) {
		job := finalizedDocFixturePG(t, storage, ctx)
		current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
		if err != nil || current.State != "completed" {
			t.Fatal(current.State, err)
		}
		current.State, current.Reason = "retrying", "temporary_failure"
		if err := storage.writeDocJob(ctx, current); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || found {
			t.Fatalf("attested from a retrying job: found=%v err=%v", found, err)
		}
	})

	t.Run("a document stored without a finalization job", func(t *testing.T) {
		repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
			t.Fatal(err)
		}
		cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + " stored directly"}}}}}
		canonical, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		hash := domain.HashContent(canonical)
		if _, err := storage.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, repo, hash); err != nil || found {
			t.Fatalf("attested a document that no job verified: found=%v err=%v", found, err)
		}
		if proof, err := storage.VerifyStoredDoc(ctx, repo, hash); err != nil || proof.Hash() != hash {
			t.Fatalf("byte verification of the same document failed: %v", err)
		}
	})

	t.Run("another repository's completed job", func(t *testing.T) {
		job := finalizedDocFixturePG(t, storage, ctx)
		other := domain.HashContent([]byte(t.Name() + time.Now().String()))
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, other, job.DocHash); found || err != nil {
			t.Fatalf("attested a document the repository does not own: found=%v err=%v", found, err)
		}
		if _, err := storage.VerifyStoredDoc(ctx, other, job.DocHash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("byte verification did not refuse the unowned document: %v", err)
		}
	})
}

// finalizedRootFixturePG publishes one conversation root through the streaming
// root finalization path and returns its completed job and fixture.
func finalizedRootFixturePG(t *testing.T, storage *PostgresStore, ctx context.Context, text string) (domain.DocFinalizationJob, rootReaderFixture) {
	t.Helper()
	ctx = rootPublicationContext(ctx)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rootFixture(t, text)
	job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, repo)
	publication, err := storage.PrepareRootDocJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Close() })
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	return job, fixture
}

func TestPGFinalizedDocReferenceAttestsCompletedRoots(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	root := func(job domain.DocFinalizationJob) domain.DocumentRef {
		return domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityRootV1}
	}

	t.Run("completed root job attests without reading chunk bodies", func(t *testing.T) {
		// Chunks are global content-addressed rows, so the damaged one must be
		// unique to this run and is restored afterwards.
		text := fmt.Sprintf("%d ", time.Now().UnixNano()) + strings.Repeat("root ", 3*domain.ConversationManifestChunkBytes/5)
		job, fixture := finalizedRootFixturePG(t, storage, ctx, text)
		if len(fixture.manifest.Chunks) < 2 {
			t.Fatal("fixture should span several chunks", len(fixture.manifest.Chunks))
		}
		proof, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash)
		if err != nil || !found || proof.DocumentRef() != root(job) {
			t.Fatalf("completed root not attested: found=%v proof=%+v err=%v", found, proof.DocumentRef(), err)
		}
		damaged := fixture.manifest.Chunks[0].Hash
		var original []byte
		if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, damaged).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, damaged, docCompress([]byte(`"damaged"`))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, damaged, original); err != nil {
				t.Error("restore damaged chunk", err)
			}
		})
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || !found {
			t.Fatalf("attestation read a root chunk body: found=%v err=%v", found, err)
		}
		if _, err := storage.VerifyStoredDoc(rootPublicationContext(ctx), job.RepoID, job.DocHash); err == nil {
			t.Fatal("byte verification accepted a damaged root chunk")
		}
	})

	t.Run("root chunk the repository no longer owns", func(t *testing.T) {
		job, fixture := finalizedRootFixturePG(t, storage, ctx, "unowned root chunk")
		if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, job.RepoID, fixture.manifest.Chunks[0].Hash); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || found {
			t.Fatalf("attested a root with an unowned chunk: found=%v err=%v", found, err)
		}
	})

	t.Run("root job that is no longer completed", func(t *testing.T) {
		job, _ := finalizedRootFixturePG(t, storage, ctx, "retrying root")
		current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
		if err != nil || current.State != "completed" {
			t.Fatal(current.State, err)
		}
		current.State, current.Reason = "retrying", "temporary_failure"
		if err := storage.writeDocJob(ctx, current); err != nil {
			t.Fatal(err)
		}
		if _, found, err := storage.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || found {
			t.Fatalf("attested from a retrying root job: found=%v err=%v", found, err)
		}
	})

	t.Run("another repository's completed root job", func(t *testing.T) {
		job, _ := finalizedRootFixturePG(t, storage, ctx, "foreign root")
		other, _ := rootPGRepo(t, rootPublicationContext(ctx), storage)
		if _, found, err := storage.FinalizedDocReference(ctx, other, job.DocHash); err != nil || found {
			t.Fatalf("attested a root the repository does not own: found=%v err=%v", found, err)
		}
	})
}

// History preparation verifies each root in its read transaction and Capture
// then reuses that reference. An attested root must be reused the same way, or
// Capture would read every chunk of the root that the attestation skipped.
func TestPGHistoryPreparationReusesAnAttestedRoot(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	text := fmt.Sprintf("%d ", time.Now().UnixNano()) + strings.Repeat("history root ", 3*domain.ConversationManifestChunkBytes/13)
	job, fixture := finalizedRootFixturePG(t, storage, ctx, text)
	if len(fixture.manifest.Chunks) < 2 {
		t.Fatal("fixture should span several chunks", len(fixture.manifest.Chunks))
	}
	ctx = rootPublicationContext(ctx)
	snap := domain.Snapshot{ID: job.DocHash, RepoID: job.RepoID, DocHash: job.DocHash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := storage.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	snap, err := storage.GetSnapshot(ctx, job.RepoID, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = historyRootReferenceTracePG{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	traced := &PostgresStore{pool: pool}
	reads := &historyRootReferenceReadsPG{}
	ctx = context.WithValue(ctx, historyRootReferenceReadKeyPG{}, reads)

	var retained *historyDocumentProofPG
	err = traced.WithinHistoryDocumentProof(ctx, job.RepoID, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
		retained = proof.(*historyDocumentProofPG)
		err := traced.WithinReadSnapshot(ctx, func(read context.Context) error {
			reads.reset()
			attested, found, err := traced.FinalizedDocReference(read, job.RepoID, job.DocHash)
			if err != nil || !found || !attested.Matches(snap) {
				t.Fatalf("finalized root not attested: found=%v err=%v", found, err)
			}
			if reads.chunks.Load() != 0 || len(retained.rootReferences) != 1 {
				t.Fatalf("attestation read %d chunk bodies, remembered %d references", reads.chunks.Load(), len(retained.rootReferences))
			}
			reads.reset()
			if err := proof.Capture(read, []domain.Snapshot{snap}); err != nil {
				return err
			}
			if reads.chunks.Load() != 0 || retained.rootReferences != nil {
				t.Fatalf("capture read %d chunk bodies of an attested root", reads.chunks.Load())
			}
			if len(retained.grants["chunk"]) != len(fixture.bodies) {
				t.Fatal("capture lost the root's chunk closure", len(retained.grants["chunk"]), len(fixture.bodies))
			}
			return nil
		})
		if err != nil {
			return err
		}
		return traced.WithinRepository(ctx, job.RepoID, proof.Pin)
	})
	if err != nil {
		t.Fatal(err)
	}

	// An attestation made outside the read transaction that Capture runs in is
	// not reused: Capture reads the root's bytes again.
	err = traced.WithinHistoryDocumentProof(ctx, job.RepoID, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
		if _, found, err := traced.FinalizedDocReference(ctx, job.RepoID, job.DocHash); err != nil || !found {
			t.Fatalf("finalized root not attested: found=%v err=%v", found, err)
		}
		return traced.WithinReadSnapshot(ctx, func(read context.Context) error {
			reads.reset()
			if err := proof.Capture(read, []domain.Snapshot{snap}); err != nil {
				return err
			}
			if reads.chunks.Load() == 0 {
				t.Fatal("capture reused an attestation from another transaction")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
