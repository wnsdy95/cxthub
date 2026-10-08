//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Each run owns distinct mutable test objects in the persistent disposable
// database; corruption fixtures must not poison a later run's global CAS.
func uniqueRootPublicationFixturePG(t *testing.T, text string) rootReaderFixture {
	t.Helper()
	nonce := time.Now().UTC().Format(time.RFC3339Nano)
	if text != "" {
		text += " " + nonce
	}
	f := rootFixture(t, text)
	f.cir.Envelope.SessionOriginID += ":" + nonce
	var err error
	f.manifest, f.bodies, err = domain.ConversationManifestForCIR(f.cir)
	if err != nil {
		t.Fatal(err)
	}
	f.hash, err = domain.ConversationManifestHash(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.manifestBytes, err = domain.CanonicalConversationManifest(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.canonical, err = domain.CanonicalBytes(f.cir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func rootPublicationClaimPG(t *testing.T, s *PostgresStore, ctx context.Context, f rootReaderFixture, repo domain.ContentHash) (domain.DocFinalizationJob, domain.VerifiedSessionDoc) {
	t.Helper()
	if repo == "" {
		repo = domain.HashContent([]byte(t.Name() + time.Now().String()))
		if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
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

func TestRootPublicationPGCurrentClosure(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{"empty", ""}, {"normal", "root <&> \uD55C"}, {"giant-repeat", strings.Repeat("x", 5*domain.ConversationManifestChunkBytes)}} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, ns := rootPGRepo(t, ctx, s)
			f := uniqueRootPublicationFixturePG(t, tc.text)
			j, doc := rootPublicationClaimPG(t, s, ctx, f, repo)
			publication, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			p := publication.(pgDocPublication)
			if err := p.stageReadBlocks(ctx, j); err != nil {
				t.Fatal(err)
			}
			assertDocNotPublishedPG(t, s, ctx, j)
			if err := publication.Complete(ctx, j, time.Now()); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, f.hash).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			raw, err = docDecompress(raw)
			if err != nil || !bytes.Equal(raw, f.manifestBytes) {
				t.Fatal("root disk bytes", err)
			}
			got, err := s.ReadVerifiedDoc(ctx, repo, f.hash)
			if err != nil {
				t.Fatal(err)
			}
			assertRootProof(t, got, f)
			done, err := s.GetDocJob(ctx, repo, j.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != doc.DocumentRef() {
				t.Fatal(done, err)
			}
			replay, err := s.EnqueueDocJob(ctx, rootPublicationJob(t, repo, f))
			if err != nil || replay.State != "completed" {
				t.Fatal(replay, err)
			}
			usage, err := s.ReadStorageUsage(ctx, ns, time.Now().Add(-time.Hour), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			want := int64(len(docCompress(f.manifestBytes)))
			for _, body := range f.bodies {
				want += int64(len(docCompress(body)))
			}
			if usage.CurrentBytes != want {
				t.Fatalf("unique compressed ownership: got=%d want=%d", usage.CurrentBytes, want)
			}
			reopened, err := NewPostgresStore(ctx, s.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.ReadVerifiedDoc(ctx, repo, f.hash); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRootPublicationPGRejectsChangedClosureAndFence(t *testing.T) {
	for _, damage := range []string{"revoked", "foreign-only", "missing", "corrupt", "winner", "expired", "reclaimed", "canceled", "wrong-reference", "recompressed"} {
		t.Run(damage, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			f := uniqueRootPublicationFixturePG(t, "owned current bytes: "+t.Name())
			j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
			publication, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			hash := f.manifest.Chunks[0].Hash
			writeCtx := ctx
			switch damage {
			case "revoked", "foreign-only", "missing":
				if _, err = s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, j.RepoID, hash); err != nil {
					t.Fatal(err)
				}
				if damage == "missing" {
					_, err = s.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, hash)
				}
				if damage == "foreign-only" {
					foreign := domain.HashContent([]byte("foreign:" + t.Name()))
					if _, err = s.PutRepo(ctx, domain.Repo{ID: foreign}); err == nil {
						_, err = s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, foreign, hash)
					}
				}
			case "corrupt":
				_, err = s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte("corrupt after proof"))
			case "winner":
				_, err = s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, f.hash, []byte("conflict winner"))
			case "expired":
				old := j
				old.LeaseUntil = time.Now().Add(-time.Second)
				err = s.writeDocJob(ctx, old)
			case "reclaimed":
				_, err = s.ClaimDocJob(ctx, j.RepoID, time.Now().Add(2*time.Minute), time.Minute)
			case "canceled":
				var cancel context.CancelFunc
				writeCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong-reference":
				j.DocIdentity = domain.DocumentIdentityLegacy
			case "recompressed":
				_, err = s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, f.bodies[hash])
			}
			if err != nil {
				t.Fatal(err)
			}
			err = publication.Complete(writeCtx, j, time.Now())
			if damage == "recompressed" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("changed closure/lease admitted")
			}
			assertDocNotPublishedPG(t, s, ctx, j)
			if damage == "revoked" || damage == "missing" || damage == "foreign-only" {
				var n int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, j.RepoID, hash).Scan(&n); err != nil || n != 0 {
					t.Fatal("revoked grant recreated", n, err)
				}
			}
			if damage == "missing" {
				var n int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, hash).Scan(&n); err != nil || n != 0 {
					t.Fatal("missing body repaired", n, err)
				}
			}
		})
	}
}

func TestRootPublicationPGRollbackAndQuota(t *testing.T) {
	for _, mode := range []string{"enclosing-rollback", "quota"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, ns := rootPGRepo(t, ctx, s)
			f := uniqueRootPublicationFixturePG(t, "")
			j, doc := rootPublicationClaimPG(t, s, ctx, f, repo)
			publication, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "quota" {
				if err := s.ConfigureStoragePolicy(ctx, ns, "root-limit-"+ns, 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "test-root-publication", "synthetic root quota"); err != nil {
					t.Fatal(err)
				}
				err = publication.Complete(ctx, j, time.Now())
				if !errors.Is(err, domain.ErrStorageLimit) {
					t.Fatal("quota publication", err)
				}
			} else {
				stop := errors.New("synthetic outer rollback")
				err = s.WithinRepository(ctx, repo, func(bound context.Context) error {
					if err := publication.Complete(bound, j, time.Now()); err != nil {
						return err
					}
					return stop
				})
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
			}
			assertDocNotPublishedPG(t, s, ctx, j)
			var n int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, f.hash).Scan(&n); err != nil || n != 0 {
				t.Fatal("root blob escaped rollback", n, err)
			}
		})
	}
}

func TestRootPublicationPGGrantAndBytesRetainedThroughCommit(t *testing.T) {
	for _, mutation := range []string{"revoke", "overwrite"} {
		t.Run(mutation, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			f := uniqueRootPublicationFixturePG(t, "retained until outer commit "+mutation)
			j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
			publication, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			var mutateDone chan error
			err = s.WithinRepository(ctx, j.RepoID, func(bound context.Context) error {
				if err := publication.Complete(bound, j, time.Now()); err != nil {
					return err
				}
				mutateDone = make(chan error, 1)
				go func() {
					probe, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					var e error
					if mutation == "revoke" {
						_, e = s.pool.Exec(probe, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, j.RepoID, f.manifest.Chunks[0].Hash)
					} else {
						_, e = s.pool.Exec(probe, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.manifest.Chunks[0].Hash, []byte("post-commit damage"))
					}
					mutateDone <- e
				}()
				select {
				case err := <-mutateDone:
					t.Errorf("mutation crossed current-byte retention: %v", err)
					mutateDone = nil
				case <-time.After(40 * time.Millisecond):
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mutateDone != nil {
				if err := <-mutateDone; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReadVerifiedDoc(ctx, j.RepoID, f.hash); err == nil {
				t.Fatal("later corruption/revocation hidden by completed job")
			}
		})
	}
}

func TestRootPublicationPGLeaseExpiresBehindWinnerLock(t *testing.T) {
	s, ctx := chunkReusePG(t)
	ctx = rootPublicationContext(ctx)
	f := uniqueRootPublicationFixturePG(t, "lease expires during root retention")
	j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
	p, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, f.hash, docCompress(f.manifestBytes)); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	if _, err := blocker.Exec(ctx, `SELECT hash FROM blobs WHERE hash=$1 FOR UPDATE`, f.hash); err != nil {
		t.Fatal(err)
	}
	j.LeaseUntil = time.Now().Add(150 * time.Millisecond)
	if err := s.writeDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Complete(ctx, j, time.Now()) }()
	time.Sleep(220 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired lock waiter published", err)
	}
	assertDocNotPublishedPG(t, s, ctx, j)
}
