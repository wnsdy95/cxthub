package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func rootPublicationProof(t *testing.T, f rootReaderFixture) domain.VerifiedSessionDoc {
	t.Helper()
	var verifier domain.CanonicalDocVerifier
	doc, err := verifier.VerifyConversationManifestDoc(context.Background(), f.hash, f.manifest, func(_ context.Context, h domain.ContentHash) ([]byte, error) { return f.bodies[h], nil })
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRootPublicationFSPreparationIsExplicitAndPure(t *testing.T) {
	for _, body := range []string{"", "root publication"} {
		s := NewFSStore(t.TempDir())
		f := rootFixture(t, body)
		doc := rootPublicationProof(t, f)
		before := rootFSImage(t, s.dataDir)
		publication, err := s.PrepareDocJob(context.Background(), doc)
		if err != nil || publication == nil {
			t.Fatalf("explicit root preparation: %v", err)
		}
		if after := rootFSImage(t, s.dataDir); len(after) != len(before) {
			t.Fatal("preparation wrote files")
		}
		if _, err := s.PutVerifiedDoc(context.Background(), domain.HashContent([]byte(t.Name())), doc); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal("legacy writer admitted root", err)
		}
	}
}

func rootPublicationJob(t *testing.T, repo domain.ContentHash, f rootReaderFixture) domain.DocFinalizationJob {
	t.Helper()
	j, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{Hash: f.hash, Identity: domain.DocumentIdentityRootV1, RootManifest: f.manifestBytes}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func rootPublicationClaimFS(t *testing.T, s *FSStore, f rootReaderFixture, optIn ...bool) (domain.DocFinalizationJob, domain.VerifiedSessionDoc) {
	t.Helper()
	ctx := rootPublicationContext(context.Background())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if len(optIn) == 0 || optIn[0] {
		if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.PutChunks(ctx, repo, f.bodies); err != nil {
		t.Fatal(err)
	}
	j := rootPublicationJob(t, repo, f)
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return claim, rootPublicationProof(t, f)
}

func TestRootPublicationFSCurrentClosure(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{"empty", ""}, {"normal", "Unicode \uD55C <&>"}, {"giant-repeat", strings.Repeat("x", 5*domain.ConversationManifestChunkBytes)}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := rootPublicationContext(context.Background())
			s := NewFSStore(t.TempDir())
			f := rootFixture(t, tc.text)
			j, doc := rootPublicationClaimFS(t, s, f)
			prepared, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.Complete(ctx, j, time.Now()); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.docPath(j.RepoID, f.hash))
			if err != nil {
				t.Fatal(err)
			}
			raw, err = docDecompress(raw)
			if err != nil || !bytes.Equal(raw, f.manifestBytes) {
				t.Fatal("root representation changed", err)
			}
			reopened := NewFSStore(s.dataDir)
			got, err := reopened.ReadVerifiedDoc(ctx, j.RepoID, f.hash)
			if err != nil {
				t.Fatal(err)
			}
			assertRootProof(t, got, f)
			done, err := reopened.GetDocJob(ctx, j.RepoID, j.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != doc.DocumentRef() {
				t.Fatal(done, err)
			}
			replay, err := reopened.EnqueueDocJob(ctx, rootPublicationJob(t, j.RepoID, f))
			if err != nil || replay.State != "completed" {
				t.Fatal(replay, err)
			}
		})
	}
}

func TestRootPublicationFSRejectsChangedDependenciesAndLease(t *testing.T) {
	for _, damage := range []string{"corrupt", "missing", "winner", "expired", "reclaimed", "canceled", "wrong-reference", "recompressed"} {
		t.Run(damage, func(t *testing.T) {
			ctx := rootPublicationContext(context.Background())
			s := NewFSStore(t.TempDir())
			f := rootFixture(t, "current bytes required")
			j, doc := rootPublicationClaimFS(t, s, f)
			prepared, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			chunk := f.manifest.Chunks[0].Hash
			switch damage {
			case "corrupt":
				err = writeAtomic(s.chunkPath(j.RepoID, chunk), docCompress([]byte("corrupt")))
			case "missing":
				err = os.Remove(s.chunkPath(j.RepoID, chunk))
			case "winner":
				err = writeAtomic(s.docPath(j.RepoID, f.hash), []byte("conflict winner"))
			case "expired":
				old := j
				old.LeaseUntil = time.Now().Add(-time.Second)
				err = s.writeDocJob(old)
			case "reclaimed":
				_, err = s.ClaimDocJob(ctx, j.RepoID, time.Now().Add(2*time.Minute), time.Minute)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong-reference":
				j.DocIdentity = domain.DocumentIdentityLegacy
			case "recompressed":
				err = writeAtomic(s.chunkPath(j.RepoID, chunk), f.bodies[chunk])
			}
			if err != nil {
				t.Fatal(err)
			}
			err = prepared.Complete(ctx, j, time.Now())
			if damage == "recompressed" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("stale or invalid publication accepted")
			}
			current, getErr := s.GetDocJob(context.Background(), j.RepoID, j.ID)
			if getErr != nil || current.State == "completed" {
				t.Fatal("completed receipt escaped", getErr)
			}
			if damage != "winner" && exists(s.docPath(j.RepoID, f.hash)) {
				t.Fatal("failed publication installed root")
			}
			if damage == "missing" && exists(s.chunkPath(j.RepoID, chunk)) {
				t.Fatal("proof repaired missing dependency")
			}
		})
	}
}

func TestRootPublicationFSPendingRetentionAndQueueReload(t *testing.T) {
	ctx := rootPublicationContext(context.Background())
	s := NewFSStore(t.TempDir())
	f := rootFixture(t, strings.Repeat("p", 3*domain.ConversationManifestChunkBytes))
	j, doc := rootPublicationClaimFS(t, s, f)
	for h := range f.bodies {
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(s.chunkPath(j.RepoID, h), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Ensure repack scans a repository with an unrelated live legacy document.
	_, legacy, _ := docJobFixture(t, j.RepoID, "legacy anchor")
	if _, err := s.PutVerifiedDoc(ctx, j.RepoID, legacy); err != nil {
		t.Fatal(err)
	}
	fsDocQueues.Delete(s.dataDir)
	reopened := NewFSStore(s.dataDir)
	q := reopened.docQueue()
	q.Lock()
	pending, err := reopened.pendingDocJobs()
	q.Unlock()
	if err != nil || len(pending) != 1 || len(pending[0].RootManifest) != 0 || len(pending[0].Manifest.Chunks) != 0 {
		t.Fatal("queue retained payload", err)
	}
	if _, _, err := reopened.RepackDocs(); err != nil {
		t.Fatal(err)
	}
	for h := range f.bodies {
		if !exists(s.chunkPath(j.RepoID, h)) {
			t.Fatal("pending root dependency swept")
		}
	}
	prepared, err := reopened.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	lock := reopened.oauthLock()
	lock.Lock()
	entered := make(chan error, 1)
	go func() { entered <- prepared.Complete(ctx, j, time.Now()) }()
	select {
	case err := <-entered:
		lock.Unlock()
		t.Fatal("completion crossed retention lock", err)
	case <-time.After(30 * time.Millisecond):
	}
	lock.Unlock()
	if err := <-entered; err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.RepackDocs(); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReadVerifiedDoc(ctx, j.RepoID, f.hash); err != nil {
		t.Fatal(err)
	}
}

func TestRootPublicationFSLeaseExpiresWhileWaitingForRetention(t *testing.T) {
	ctx := rootPublicationContext(context.Background())
	s := NewFSStore(t.TempDir())
	f := rootFixture(t, "expired before root becomes visible")
	j, doc := rootPublicationClaimFS(t, s, f)
	j.LeaseUntil = time.Now().Add(80 * time.Millisecond)
	if err := s.writeDocJob(j); err != nil {
		t.Fatal(err)
	}
	p, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	retention := s.oauthLock()
	retention.Lock()
	done := make(chan error, 1)
	go func() { done <- p.Complete(ctx, j, time.Now()) }()
	time.Sleep(120 * time.Millisecond)
	retention.Unlock()
	if err := <-done; !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired publication", err)
	}
	if exists(s.docPath(j.RepoID, f.hash)) {
		t.Fatal("expired waiter installed a root")
	}
}

func rootPublicationContext(ctx context.Context) context.Context {
	ids := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	return outbound.WithDocumentIdentityCompatibility(ctx, ids, ids)
}
