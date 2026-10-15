//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func prepareLegacyJobPG(t *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob) *pgLegacyStreamingPublication {
	t.Helper()
	prepared, err := storage.PrepareLegacyDocJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	publication := prepared.(*pgLegacyStreamingPublication)
	t.Cleanup(func() {
		if err := publication.Close(); err != nil {
			t.Error(err)
		}
	})
	return publication
}

func TestLegacyStreamingPGPublicationAndCleanup(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	job, expected, _ := preparedJobFixturePG(t, storage, ctx)
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	spoolPath := publication.spool.file.Name()
	info, err := os.Stat(spoolPath)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() == 0 {
		t.Fatal("invalid private spool", info, err)
	}
	assertDocNotPublishedPG(t, storage, ctx, job)
	if err := publication.Stage(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	assertDocNotPublishedPG(t, storage, ctx, job)
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	actual, err := storage.GetDoc(ctx, job.RepoID, job.DocHash)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := domain.CanonicalBytes(actual.CIR)
	if err != nil || !bytes.Equal(canonical, expected.Bytes()) {
		t.Fatal("legacy identity or transcript changed", err)
	}
	index, err := storage.DocReadIndex(ctx, job.RepoID, job.DocHash)
	want, _ := expected.ReadIndex()
	if err != nil || len(index.Events) != len(want.Events) {
		t.Fatal("index not published", err)
	}
	for ordinal, event := range index.Events {
		want.Events[ordinal].Text = ""
		if event != want.Events[ordinal] {
			t.Fatal("projection changed", ordinal)
		}
	}
	current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || current.State != "completed" || current.Version != job.Version {
		t.Fatal("receipt did not commit", current, err)
	}
	stageCountPG(t, storage, ctx, 0, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1 AND job_id=$2`, job.RepoID, job.ID)
	if err := publication.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatal("spool not removed", err)
	}
}

func TestLegacyStreamingPGCurrentFences(t *testing.T) {
	for _, change := range []string{"corruption", "revoked", "expired", "reclaimed", "canceled", "spool"} {
		t.Run(change, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			job, _, chunks := preparedJobFixturePG(t, storage, ctx)
			publication := prepareLegacyJobPG(t, storage, ctx, job)
			if err := publication.Stage(ctx, job, time.Minute); err != nil {
				t.Fatal(err)
			}
			write := ctx
			want := domain.ErrConflict
			switch change {
			case "corruption":
				_, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunks.Order[0], []byte("damaged"))
				if err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
			case "revoked":
				_, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk'`, job.RepoID)
				if err != nil {
					t.Fatal(err)
				}
				want = domain.ErrNotFound
			case "expired":
				current := job
				current.LeaseUntil = time.Now().Add(-time.Second)
				if err := storage.writeDocJob(ctx, current); err != nil {
					t.Fatal(err)
				}
			case "reclaimed":
				if _, err := storage.ClaimDocJob(ctx, job.RepoID, time.Now().Add(2*time.Minute), time.Minute); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				write, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "spool":
				if _, err := publication.spool.file.WriteAt([]byte("!"), 0); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
			}
			if err := publication.Complete(write, job, time.Now()); !errors.Is(err, want) {
				t.Fatalf("completion error %v, want %v", err, want)
			}
			assertDocNotPublishedPG(t, storage, ctx, job)
		})
	}
}

func TestLegacyStreamingPGFailureCleansPreparation(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	job, _, chunks := preparedJobFixturePG(t, storage, ctx)
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunks.Order[len(chunks.Order)-1], []byte("damaged")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PrepareLegacyDocJob(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "cxthub-legacy-doc-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("failed preparation leaked spool", paths, err)
	}
	assertDocNotPublishedPG(t, storage, ctx, job)
}

func TestLegacyStreamingPGStageHonorsLease(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	job, _, _ := preparedJobFixturePG(t, storage, ctx)
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	expired := job
	expired.LeaseUntil = time.Now().Add(-time.Second)
	if err := storage.writeDocJob(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if err := publication.Stage(ctx, job, time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	stageCountPG(t, storage, ctx, 0, `SELECT count(*) FROM doc_read_block_preparations_v3 WHERE repo_id=$1 AND job_id=$2`, job.RepoID, job.ID)
	assertDocNotPublishedPG(t, storage, ctx, job)
}

func TestLegacyStreamingPGExistingRepresentations(t *testing.T) {
	for _, representation := range []string{"raw", "v1", "repartitioned-v2", "corrupt"} {
		t.Run(representation, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			job, doc, plan := preparedJobFixturePG(t, storage, ctx)
			owner := domain.HashContent([]byte(string(job.RepoID) + " other owner"))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: owner}); err != nil {
				t.Fatal(err)
			}
			raw := doc.Bytes()
			var manifest domain.DocChunkManifest
			if representation == "v1" {
				var decoded struct {
					Events []json.RawMessage `json:"events"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				manifest = domain.DocChunkManifest{Format: domain.ChunkFormatV1, Envelope: plan.Manifest.Envelope}
				for _, event := range decoded.Events {
					hash := domain.HashContent(event)
					manifest.Chunks = append(manifest.Chunks, hash)
					if _, _, err := storage.PutChunks(ctx, owner, map[domain.ContentHash][]byte{hash: event}); err != nil {
						t.Fatal(err)
					}
				}
			} else if representation == "repartitioned-v2" {
				manifest = domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: plan.Manifest.Envelope}
				for _, hash := range plan.Order {
					body := plan.Bodies[hash]
					for offset := 0; offset < len(body); offset += 11 {
						part := body[offset:min(offset+11, len(body))]
						partHash := domain.HashContent(part)
						manifest.Chunks = append(manifest.Chunks, partHash)
						if _, _, err := storage.PutChunks(ctx, owner, map[domain.ContentHash][]byte{partHash: part}); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else if representation == "corrupt" {
				raw = []byte("corrupt existing blob")
			}
			if manifest.Format != "" {
				var err error
				raw, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, job.DocHash, docCompress(raw)); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, owner, job.DocHash); err != nil {
				t.Fatal(err)
			}
			publication := prepareLegacyJobPG(t, storage, ctx, job)
			if err := publication.Stage(ctx, job, time.Minute); err != nil {
				t.Fatal(err)
			}
			err := publication.Complete(ctx, job, time.Now())
			if representation == "corrupt" {
				if !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt representation accepted", err)
				}
				assertDocNotPublishedPG(t, storage, ctx, job)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, repo := range []domain.ContentHash{owner, job.RepoID} {
				proof, err := storage.VerifyStoredDoc(ctx, repo, job.DocHash)
				if err != nil || proof.Hash() != job.DocHash {
					t.Fatal("existing owner lost readable content", err)
				}
			}
		})
	}
}

func TestLegacyStreamingPGDoesNotRepairExistingArchive(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	_, doc, plan := docJobFixture(t, repo, string(repo))
	manifest := domain.DocChunkManifest{Format: domain.ChunkFormatV2, Envelope: plan.Manifest.Envelope}
	for _, hash := range plan.Order {
		body := plan.Bodies[hash]
		for offset := 0; offset < len(body); offset += 13 {
			part := body[offset:min(offset+13, len(body))]
			partHash := domain.HashContent(part)
			manifest.Chunks = append(manifest.Chunks, partHash)
			if _, _, err := storage.PutChunks(ctx, repo, map[domain.ContentHash][]byte{partHash: part}); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, doc.Hash(), docCompress(raw)); err != nil {
		t.Fatal(err)
	}
	job, err := domain.NewDocFinalizationJob(repo, doc.Hash(), manifest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = storage.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publication := prepareLegacyJobPG(t, storage, ctx, job)
	if err := publication.Stage(ctx, job, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := publication.Complete(ctx, job, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("silently repaired published archive", err)
	}
	assertDocNotPublishedPG(t, storage, ctx, job)
}
