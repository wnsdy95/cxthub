package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func streamingRootClaimFS(t *testing.T, storage *FSStore, repo domain.ContentHash, fixture rootReaderFixture, optIn bool) domain.DocFinalizationJob {
	t.Helper()
	ctx := rootPublicationContext(context.Background())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if optIn {
		if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := storage.PutChunks(ctx, repo, fixture.bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(ctx, rootPublicationJob(t, repo, fixture)); err != nil {
		t.Fatal(err)
	}
	job, err := storage.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func streamingRootPrepareFS(t *testing.T, storage *FSStore, job domain.DocFinalizationJob) *fsStreamingRootPublication {
	t.Helper()
	publication, err := storage.PrepareRootDocJob(rootPublicationContext(context.Background()), job)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publication.Close(); err != nil {
			t.Error(err)
		}
	})
	return publication.(*fsStreamingRootPublication)
}

func streamingRootCloseFS(t *testing.T, publication *fsStreamingRootPublication, spoolDirectory string) {
	t.Helper()
	if err := publication.Close(); err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
}

func assertStreamingRootPendingFS(t *testing.T, storage *FSStore, job domain.DocFinalizationJob) {
	t.Helper()
	current, err := storage.GetDocJob(context.Background(), job.RepoID, job.ID)
	if err != nil || current.State == "completed" {
		t.Fatal("failed publication completed receipt", current, err)
	}
	if exists(storage.rangeIndexPath(job.RepoID, job.DocHash)) {
		t.Fatal("failed publication exposed range header")
	}
}

func TestStreamingRootFSSuccess(t *testing.T) {
	for _, mode := range []string{"empty", "pages", "repeated-chunks", "existing-root", "renewed", "reencoded"} {
		t.Run(mode, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			repo := domain.HashContent([]byte(t.Name()))
			fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
			if mode == "empty" {
				fixture = rootFixture(t, "")
			} else if mode == "repeated-chunks" {
				fixture = rootFixture(t, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
			}
			job := streamingRootClaimFS(t, storage, repo, fixture, true)
			if mode == "existing-root" {
				if err := writeAtomic(storage.docPath(repo, fixture.hash), fixture.manifestBytes); err != nil {
					t.Fatal(err)
				}
			}
			before := rootFSImage(t, storage.dataDir)
			publication := streamingRootPrepareFS(t, storage, job)
			if after := rootFSImage(t, storage.dataDir); !reflect.DeepEqual(before, after) {
				t.Fatal("preparation modified durable storage")
			}
			info, err := publication.prepared.spool.file.Stat()
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("spool permissions", info, err)
			}
			if mode == "renewed" {
				if err := storage.RenewDocJob(ctx, job, time.Now().UTC(), 2*time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "reencoded" {
				for hash, body := range fixture.bodies {
					if err := writeAtomic(storage.chunkPath(repo, hash), body); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				t.Fatal(err)
			}
			streamingRootCloseFS(t, publication, spoolDirectory)
			if err := publication.Close(); err != nil {
				t.Fatal("idempotent close", err)
			}
			if len(storage.docProofs.proofs) != 0 {
				t.Fatal("streaming publication retained an owned document proof")
			}
			reopened := NewFSStore(storage.dataDir)
			assertRangePages(t, ctx, reopened, repo, fixture)
			done, err := reopened.GetDocJob(ctx, repo, job.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != job.DocumentRef() || !done.LeaseUntil.IsZero() || done.Version != job.Version {
				t.Fatal("completed receipt", done, err)
			}
			raw, err := os.ReadFile(storage.docPath(repo, fixture.hash))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "existing-root" && !bytes.Equal(raw, fixture.manifestBytes) {
				t.Fatal("existing matching root was overwritten")
			}
			decoded, err := docDecompress(raw)
			if err != nil || !bytes.Equal(decoded, fixture.manifestBytes) {
				t.Fatal("published root differs", err)
			}
		})
	}
}

func TestStreamingRootFSPrepareFailure(t *testing.T) {
	for _, mode := range []string{"canceled", "missing-chunk", "corrupt-chunk", "oversized-chunk", "foreign-chunks"} {
		t.Run(mode, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			fixture := rootFixture(t, "owned current bytes")
			repo := domain.HashContent([]byte(t.Name()))
			job := streamingRootClaimFS(t, storage, repo, fixture, true)
			want := domain.ErrNotFound
			var err error
			switch mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "missing-chunk":
				err = os.Remove(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash))
			case "corrupt-chunk":
				err = writeAtomic(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash), []byte("corrupt"))
				want = domain.ErrIntegrity
			case "oversized-chunk":
				err = os.Truncate(storage.chunkPath(repo, fixture.manifest.Chunks[0].Hash), int64(maxStoredRangeChunkBytes+1))
				want = domain.ErrIntegrity
			case "foreign-chunks":
				job = rootPublicationJob(t, domain.HashContent([]byte("foreign")), fixture).Claim(time.Now().UTC(), time.Minute)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := rootFSImage(t, storage.dataDir)
			publication, err := storage.PrepareRootDocJob(ctx, job)
			if publication != nil {
				_ = publication.Close()
				t.Fatal("failed preparation returned a publication")
			}
			if !errors.Is(err, want) {
				t.Fatalf("preparation got %v, want %v", err, want)
			}
			if after := rootFSImage(t, storage.dataDir); !reflect.DeepEqual(before, after) {
				t.Fatal("failed preparation modified storage")
			}
			assertRangeSpoolsRemoved(t, spoolDirectory)
		})
	}
}

func TestStreamingRootFSCompletionFailure(t *testing.T) {
	for _, mode := range []string{"corrupt-chunk", "wrong-length", "oversized-chunk", "missing-chunk", "corrupt-root", "different-root", "expired", "reclaimed", "wrong-job", "wrong-version", "canceled", "page-write", "spool-read"} {
		t.Run(mode, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			fixture := rangeFixture(t, domain.DocRangePageEvents+1)
			repo := domain.HashContent([]byte(t.Name()))
			job := streamingRootClaimFS(t, storage, repo, fixture, true)
			publication := streamingRootPrepareFS(t, storage, job)
			requested := job
			chunk := fixture.manifest.Chunks[0].Hash
			var err, want error
			switch mode {
			case "corrupt-chunk":
				body := bytes.Clone(fixture.bodies[chunk])
				body[len(body)-1] ^= 1
				err = writeAtomic(storage.chunkPath(repo, chunk), docCompress(body))
				want = domain.ErrIntegrity
			case "wrong-length":
				err = writeAtomic(storage.chunkPath(repo, chunk), docCompress(fixture.bodies[chunk][:len(fixture.bodies[chunk])-1]))
				want = domain.ErrIntegrity
			case "oversized-chunk":
				err = os.Truncate(storage.chunkPath(repo, chunk), int64(maxStoredRangeChunkBytes+1))
				want = domain.ErrIntegrity
			case "missing-chunk":
				err = os.Remove(storage.chunkPath(repo, chunk))
				want = domain.ErrNotFound
			case "corrupt-root":
				err = writeAtomic(storage.docPath(repo, fixture.hash), []byte("corrupt root"))
				want = domain.ErrIntegrity
			case "different-root":
				err = writeAtomic(storage.docPath(repo, fixture.hash), docCompress(rootFixture(t, "different root").manifestBytes))
				want = domain.ErrIntegrity
			case "expired":
				old := job
				old.LeaseUntil = time.Now().Add(-time.Second)
				err = storage.writeDocJob(old)
				want = domain.ErrConflict
			case "reclaimed":
				_, err = storage.ClaimDocJob(ctx, repo, job.LeaseUntil.Add(time.Second), time.Minute)
				want = domain.ErrConflict
			case "wrong-job":
				requested = rootPublicationJob(t, repo, rootFixture(t, "another job")).Claim(time.Now().UTC(), time.Minute)
			case "wrong-version":
				requested.Version++
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "page-write":
				header := publication.prepared.root.ranges.Header()
				err = os.MkdirAll(storage.rangePagePath(repo, fixture.hash, header.Pages[0]), 0700)
			case "spool-read":
				err = publication.prepared.spool.file.Truncate(0)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := rootFSImage(t, storage.dataDir)
			err = publication.Complete(ctx, requested, time.Unix(0, 0))
			if err == nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("completion got %v, want rejection %v", err, want)
			}
			if after := rootFSImage(t, storage.dataDir); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected publication changed root, dependencies, pages or receipt")
			}
			assertStreamingRootPendingFS(t, storage, job)
			streamingRootCloseFS(t, publication, spoolDirectory)
		})
	}
}

func TestStreamingRootFSCrossRepositoryPlan(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	ctx := rootPublicationContext(context.Background())
	storage := NewFSStore(t.TempDir())
	fixture := rootFixture(t, "shared bytes do not share publication authority")
	job := streamingRootClaimFS(t, storage, domain.HashContent([]byte("source")), fixture, true)
	foreign := streamingRootClaimFS(t, storage, domain.HashContent([]byte("foreign")), fixture, true)
	publication := streamingRootPrepareFS(t, storage, job)
	before := rootFSImage(t, storage.dataDir)
	if err := publication.Complete(ctx, foreign, time.Now()); err == nil {
		t.Fatal("plan completed another repository's valid job")
	}
	if after := rootFSImage(t, storage.dataDir); !reflect.DeepEqual(before, after) {
		t.Fatal("cross-repository completion modified storage")
	}
	assertStreamingRootPendingFS(t, storage, job)
	assertStreamingRootPendingFS(t, storage, foreign)
	streamingRootCloseFS(t, publication, spoolDirectory)
}

func TestStreamingRootFSCloseWithoutCompletion(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	storage := NewFSStore(t.TempDir())
	fixture := rangeFixture(t, domain.DocRangePageEvents+1)
	job := streamingRootClaimFS(t, storage, domain.HashContent([]byte(t.Name())), fixture, true)
	publication := streamingRootPrepareFS(t, storage, job)
	before := rootFSImage(t, storage.dataDir)
	for attempt := 0; attempt < 2; attempt++ {
		if err := publication.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
	if after := rootFSImage(t, storage.dataDir); !reflect.DeepEqual(before, after) {
		t.Fatal("close modified durable storage")
	}
	assertStreamingRootPendingFS(t, storage, job)
}

func TestStreamingRootFSCurrentPublicationPolicy(t *testing.T) {
	for _, mode := range []string{"no-opt-in", "missing-declaration", "opt-in-after-prepare"} {
		t.Run(mode, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			fixture := rootFixture(t, "current repository policy")
			job := streamingRootClaimFS(t, storage, domain.HashContent([]byte(t.Name())), fixture, mode == "missing-declaration")
			publication := streamingRootPrepareFS(t, storage, job)
			if mode == "missing-declaration" {
				ctx = context.Background()
			} else if mode == "opt-in-after-prepare" {
				if err := storage.RequireDocumentIdentity(ctx, job.RepoID, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			}
			err := publication.Complete(ctx, job, time.Now())
			if mode == "opt-in-after-prepare" {
				if err != nil {
					t.Fatal(err)
				}
				assertRangePages(t, ctx, storage, job.RepoID, fixture)
			} else {
				if !errors.Is(err, domain.ErrRootPublicationDisabled) && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
					t.Fatal("publication bypassed policy", err)
				}
				assertStreamingRootPendingFS(t, storage, job)
				if exists(storage.docPath(job.RepoID, fixture.hash)) {
					t.Fatal("policy rejection published root")
				}
			}
			streamingRootCloseFS(t, publication, spoolDirectory)
		})
	}
}

func TestStreamingRootFSLeaseExpiresWaitingForLocks(t *testing.T) {
	for _, mode := range []string{"retention", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			ctx := rootPublicationContext(context.Background())
			storage := NewFSStore(t.TempDir())
			fixture := rootFixture(t, "lease expires while publication waits")
			job := streamingRootClaimFS(t, storage, domain.HashContent([]byte(t.Name())), fixture, true)
			publication := streamingRootPrepareFS(t, storage, job)
			job.LeaseUntil = time.Now().Add(100 * time.Millisecond)
			if err := storage.writeDocJob(job); err != nil {
				t.Fatal(err)
			}
			lock := storage.oauthLock()
			if mode == "metadata" {
				lock = storage.refLock(job.RepoID, domain.RefBranch, "")
			}
			lock.Lock()
			done := make(chan error, 1)
			go func() { done <- publication.Complete(ctx, job, time.Unix(0, 0)) }()
			select {
			case err := <-done:
				lock.Unlock()
				t.Fatal("publication crossed held lock", err)
			case <-time.After(20 * time.Millisecond):
			}
			time.Sleep(time.Until(job.LeaseUntil) + 10*time.Millisecond)
			lock.Unlock()
			select {
			case err := <-done:
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatal("expired completion", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("completion did not release locks")
			}
			assertStreamingRootPendingFS(t, storage, job)
			if exists(storage.docPath(job.RepoID, fixture.hash)) {
				t.Fatal("expired claim published root")
			}
			streamingRootCloseFS(t, publication, spoolDirectory)
		})
	}
}

func TestStreamingRootFSHeaderFailureRecovery(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	ctx := rootPublicationContext(context.Background())
	storage := NewFSStore(t.TempDir())
	fixture := rangeFixture(t, domain.DocRangePageEvents+1)
	job := streamingRootClaimFS(t, storage, domain.HashContent([]byte(t.Name())), fixture, true)
	publication := streamingRootPrepareFS(t, storage, job)
	blocked := storage.rangeIndexPath(job.RepoID, job.DocHash)
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := publication.Complete(ctx, job, time.Now()); err == nil {
		t.Fatal("header failure acknowledged completion")
	}
	streamingRootCloseFS(t, publication, spoolDirectory)
	if !exists(storage.docPath(job.RepoID, fixture.hash)) {
		t.Fatal("root must precede range header publication")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	assertStreamingRootPendingFS(t, storage, job)
	reopened := NewFSStore(storage.dataDir)
	retry := streamingRootPrepareFS(t, reopened, job)
	if err := retry.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal("reprepared completion did not recover", err)
	}
	streamingRootCloseFS(t, retry, spoolDirectory)
	assertRangePages(t, ctx, reopened, job.RepoID, fixture)
}
