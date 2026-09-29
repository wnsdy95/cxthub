//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func preparedJobFixturePG(t *testing.T, s *PostgresStore, ctx context.Context) (domain.DocFinalizationJob, domain.VerifiedSessionDoc, domain.DocChunkPlan) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	j, doc, chunks := docJobFixture(t, repo, string(repo)+" publication")
	if _, _, err := s.PutChunks(ctx, repo, chunks.Bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return claim, doc, chunks
}

func assertDocNotPublishedPG(t *testing.T, s *PostgresStore, ctx context.Context, j domain.DocFinalizationJob) {
	t.Helper()
	job, err := s.GetDocJob(ctx, j.RepoID, j.ID)
	if err != nil || job.State != "running" {
		t.Fatalf("completion escaped: %+v %v", job, err)
	}
	if have, err := s.HasDocs(ctx, j.RepoID, []domain.ContentHash{j.DocHash}); err != nil || len(have) != 0 {
		t.Fatalf("ownership escaped: %v %v", have, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_indexes_v3 WHERE hash=$1`, j.DocHash).Scan(&count); err != nil || count != 0 {
		t.Fatalf("index escaped: %d %v", count, err)
	}
}

func TestPGDocPreparationDoesNotWaitForRepositoryWriter(t *testing.T) {
	s, ctx := chunkReusePG(t)
	j, doc, _ := preparedJobFixturePG(t, s, ctx)
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithinRepository(ctx, j.RepoID, func(context.Context) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	prepareCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	publication, err := s.PrepareDocJob(prepareCtx, doc)
	cancel()
	close(release)
	if writerErr := <-done; writerErr != nil {
		t.Fatal(writerErr)
	}
	if err != nil || publication == nil {
		t.Fatalf("CPU preparation waited behind repository writer: %v", err)
	}
	assertDocNotPublishedPG(t, s, ctx, j)
	// Preparation does not retain its canceled request context or an open tx.
	if err := publication.Complete(ctx, j, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPGPreparedDocRechecksLeaseAndCurrentChunkBytes(t *testing.T) {
	for _, change := range []string{"expired", "reclaimed", "corrupted", "collected", "canceled"} {
		t.Run(change, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			j, doc, chunks := preparedJobFixturePG(t, s, ctx)
			publication, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			writeCtx := ctx
			want := domain.ErrConflict
			switch change {
			case "expired":
				expired := j
				expired.LeaseUntil = time.Now().Add(-time.Second)
				if err := s.writeDocJob(ctx, expired); err != nil {
					t.Fatal(err)
				}
			case "reclaimed":
				if _, err := s.ClaimDocJob(ctx, j.RepoID, time.Now().Add(2*time.Minute), time.Minute); err != nil {
					t.Fatal(err)
				}
			case "corrupted":
				if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunks.Order[0], []byte("corrupted after preparation")); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
			case "collected":
				// Simulate missing storage after validation: the verified immutable
				// body can reconstruct it, but ownership must wait for completion.
				if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk'`, j.RepoID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, chunks.Order[0]); err != nil {
					t.Fatal(err)
				}
				want = nil
			case "canceled":
				var cancel context.CancelFunc
				writeCtx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			err = publication.Complete(writeCtx, j, time.Now())
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if want != nil {
				assertDocNotPublishedPG(t, s, ctx, j)
			} else if got, err := s.GetDoc(ctx, j.RepoID, doc.Hash()); err != nil || got.Hash != doc.Hash() {
				t.Fatalf("reconstructed body %v", err)
			}
		})
	}
}

func TestPGPreparedDocDeferredQuotaRollsBackCompletion(t *testing.T) {
	s, ctx := chunkReusePG(t)
	name := fmt.Sprintf("docquota%d", time.Now().UnixNano())
	u := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	if err := s.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	ns := domain.Namespace{ID: domain.NewID("ns_"), Slug: name, Kind: domain.NamespaceUser, UserID: u.ID, CreatedAt: time.Now().UTC()}
	if err := s.CreateNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	r := domain.Repository{ID: domain.NewID("ws_"), Name: name, Slug: "prepared", OwnerID: u.ID, OwnerUsername: name, OwnerNamespaceID: ns.ID, CreatedAt: time.Now().UTC()}
	if err := s.CreateRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(r.ID))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: r.ID}); err != nil {
		t.Fatal(err)
	}
	j, doc, _ := docJobFixture(t, repo, string(repo)+" quota")
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	// Policy may change while pure preparation is running.
	if err := s.ConfigureStoragePolicy(ctx, ns.ID, "limit-"+ns.ID, 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, u.ID, "prepared publication quota"); err != nil {
		t.Fatal(err)
	}
	if err := publication.Complete(ctx, claim, time.Now()); !errors.Is(err, domain.ErrStorageLimit) {
		t.Fatalf("deferred quota %v", err)
	}
	assertDocNotPublishedPG(t, s, ctx, j)
	usage, err := s.ReadStorageUsage(ctx, ns.ID, time.Now().Add(-time.Hour), time.Now())
	if err != nil || usage.CurrentBytes != 0 {
		t.Fatalf("usage escaped rollback: %+v %v", usage, err)
	}
}
