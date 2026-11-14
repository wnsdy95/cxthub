//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
