//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGStoredDocProofOwnershipRollbackAndCorruption(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	doc, v := verifiedDocFixture(t)
	stop := errors.New("rollback document grant")
	err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, err := s.PutVerifiedDoc(tx, repo, v); err != nil {
			return err
		}
		if _, err := s.VerifyStoredDoc(tx, repo, doc.Hash); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("proof escaped rolled-back grant", err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if p, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); err != nil || !p.Matches(domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash}) {
			t.Fatal("valid proof", p, err)
		}
	}
	if _, err := s.VerifyStoredDoc(ctx, domain.HashContent([]byte("other")), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cross-repo proof", err)
	}
	// A current ownership grant is required even after an earlier warm read.
	if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("stale grant cached", err)
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, v); err != nil {
		t.Fatal(err)
	}
	// The fixture content address is shared with other store tests. Restore its
	// physical bytes before closing the pool so corruption cannot leak to them.
	var original []byte
	if err := s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, doc.Hash).Scan(&original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.pool.Exec(context.Background(), `UPDATE blobs SET bytes=$1 WHERE hash=$2`, original, doc.Hash); err != nil {
			t.Error(err)
		}
	}()
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$1 WHERE hash=$2`, []byte(`{"events":[]}`), doc.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyStoredDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("warm proof hid corruption", err)
	}
}

// Put a unique marker throughout the event body, not just in its envelope:
// globally addressed chunks must not alias fixtures corrupted by other tests.
func storedDocChunkFixturePG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash, domain.VerifiedSessionDoc, domain.DocChunkPlan) {
	t.Helper()
	s, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().UTC().Format(time.RFC3339Nano)))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo)},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("owned chunk "+string(repo)+" \uD55C\uAE00 ", 8000)}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Bodies) < 2 {
		t.Fatal("fixture must contain multiple distinct chunks")
	}
	for _, body := range plan.Bodies {
		if !bytes.Contains(body, []byte(repo)) {
			t.Fatal("fixture chunk lacks its unique identity")
		}
	}
	if _, err := s.PutVerifiedDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	// Restore corruption/removed rows before the shared test pool closes. The
	// identities are unique, but later integrity scans must not inherit damage.
	stored := make(map[domain.ContentHash][]byte, len(plan.Bodies)+1)
	for _, hash := range append([]domain.ContentHash{doc.Hash()}, plan.Order...) {
		var body []byte
		if err := s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&body); err != nil {
			t.Fatal(err)
		}
		stored[hash] = body
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for hash, body := range stored {
			if _, err := s.pool.Exec(cleanup, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)
 ON CONFLICT(hash) DO UPDATE SET bytes=EXCLUDED.bytes`, hash, body); err != nil {
				t.Error("restore fixture bytes", err)
			}
		}
		for hash := range plan.Bodies {
			if _, err := s.pool.Exec(cleanup, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, repo, hash); err != nil {
				t.Error("restore fixture chunk ownership", err)
			}
		}
	})
	return s, ctx, repo, doc, plan
}

func requireStoredDocProofPG(t *testing.T, s *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) {
	t.Helper()
	// The first read admits changed physical bytes; the second exercises reuse.
	for i := 0; i < 2; i++ {
		proof, err := s.VerifyStoredDoc(ctx, repo, hash)
		if err != nil || !proof.Matches(domain.Snapshot{ID: hash, DocHash: hash}) {
			t.Fatalf("verification %d: proof=%v error=%v", i, proof, err)
		}
	}
}

func TestPGStoredDocChunkProofAcceptsPhysicalRepacking(t *testing.T) {
	s, ctx, repo, doc, plan := storedDocChunkFixturePG(t)
	requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	// Repack chunks and manifest independently so both mixed encodings are
	// covered. Content IDs and ownership remain the same throughout.
	for _, step := range []struct {
		name       string
		chunks     bool
		compressed bool
	}{
		{"uncompressed chunks", true, false},
		{"uncompressed manifest", false, false},
		{"compressed chunks", true, true},
		{"compressed manifest", false, true},
	} {
		t.Run(step.name, func(t *testing.T) {
			bodies := map[domain.ContentHash][]byte{doc.Hash(): manifest}
			if step.chunks {
				bodies = plan.Bodies
			}
			for hash, body := range bodies {
				if step.compressed {
					body = docCompress(body)
				}
				var previous []byte
				if err := s.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&previous); err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(previous, body) {
					t.Fatal("repack did not change physical bytes")
				}
				if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, body); err != nil {
					t.Fatal(err)
				}
			}
			requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
		})
	}
}

func TestPGStoredDocChunkProofRechecksCurrentOwnershipAndBytes(t *testing.T) {
	for _, damage := range []string{"revoked chunk grant", "missing chunk", "uncompressed corrupt chunk", "compressed corrupt chunk", "reordered manifest"} {
		t.Run(damage, func(t *testing.T) {
			s, ctx, repo, doc, plan := storedDocChunkFixturePG(t)
			requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
			// Damage a nonfirst chunk to catch proofs checking just the first body.
			hash := plan.Order[len(plan.Order)-1]
			want := domain.ErrIntegrity
			switch damage {
			case "revoked chunk grant", "missing chunk":
				want = domain.ErrNotFound
				if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil {
					t.Fatal(err)
				}
				if damage == "missing chunk" {
					// Remove the FK grant first; never disable database integrity.
					if _, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, hash); err != nil {
						t.Fatal(err)
					}
				}
				var present bool
				if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=$1)`, hash).Scan(&present); err != nil || present != (damage == "revoked chunk grant") {
					t.Fatal("fixture must distinguish missing ownership from missing bytes", present, err)
				}
			case "uncompressed corrupt chunk", "compressed corrupt chunk":
				body := bytes.Clone(plan.Bodies[hash])
				body[len(body)/2] ^= 1 // Same-length change under the existing content ID.
				if damage == "compressed corrupt chunk" {
					body = docCompress(body)
				}
				if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, body); err != nil {
					t.Fatal(err)
				}
			case "reordered manifest":
				manifest := plan.Manifest
				manifest.Chunks = append([]domain.ContentHash(nil), manifest.Chunks...)
				last := len(manifest.Chunks) - 1
				manifest.Chunks[0], manifest.Chunks[last] = manifest.Chunks[last], manifest.Chunks[0]
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, doc.Hash(), docCompress(raw)); err != nil {
					t.Fatal(err)
				}
			}
			var docOwned bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2)`, repo, doc.Hash()).Scan(&docOwned); err != nil || !docOwned {
				t.Fatal("chunk failure must retain the document grant", err)
			}
			proof, err := s.VerifyStoredDoc(ctx, repo, doc.Hash())
			if !errors.Is(err, want) || proof.Valid() {
				t.Fatalf("warm proof hid %s: proof=%v error=%v, want %v", damage, proof, err, want)
			}
		})
	}
}

func TestPGStoredDocChunkProofUsesTransactionOwnership(t *testing.T) {
	t.Run("uncommitted revocation", func(t *testing.T) {
		s, ctx, repo, doc, plan := storedDocChunkFixturePG(t)
		hash := plan.Order[len(plan.Order)-1]
		requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
		stop := errors.New("rollback chunk ownership")
		// A pooled ownership query would see the committed grant and miss this
		// transaction's revocation, incorrectly accepting its already warm proof.
		err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
			if _, err := s.db(bound).Exec(bound, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil {
				return err
			}
			proof, err := s.VerifyStoredDoc(bound, repo, doc.Hash())
			if !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
				t.Errorf("warm proof ignored transaction-local revocation: %v %v", proof, err)
			}
			return stop
		})
		if !errors.Is(err, stop) {
			t.Fatal(err)
		}
		requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
	})

	t.Run("rolled back grant", func(t *testing.T) {
		s, ctx, repo, doc, plan := storedDocChunkFixturePG(t)
		hash := plan.Order[len(plan.Order)-1]
		stop := errors.New("rollback chunk ownership")
		// Keep the doc grant and global bytes committed while only a chunk grant is
		// missing. First warm the physical proof inside the transaction, then roll
		// its chunk grant back without changing any of the proof's stored bytes.
		if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil {
			t.Fatal(err)
		}
		err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
			if _, err := s.db(bound).Exec(bound, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, hash); err != nil {
				return err
			}
			for i := 0; i < 2; i++ {
				proof, err := s.VerifyStoredDoc(bound, repo, doc.Hash())
				if err != nil {
					return err
				}
				if !proof.Matches(domain.Snapshot{ID: doc.Hash(), DocHash: doc.Hash()}) {
					t.Error("transaction-local grant produced the wrong document proof")
				}
			}
			return stop
		})
		if !errors.Is(err, stop) {
			t.Fatal("transaction-local chunk grant was not visible", err)
		}
		var docOwned, chunkOwned, chunkPresent bool
		if err := s.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2),
 EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$3),
 EXISTS(SELECT 1 FROM blobs WHERE hash=$3)`, repo, doc.Hash(), hash).Scan(&docOwned, &chunkOwned, &chunkPresent); err != nil || !docOwned || chunkOwned || !chunkPresent {
			t.Fatal("rollback must revoke only chunk ownership", docOwned, chunkOwned, chunkPresent, err)
		}
		proof, err := s.VerifyStoredDoc(ctx, repo, doc.Hash())
		if !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
			t.Fatal("warm proof escaped rolled-back chunk grant", proof, err)
		}
		if _, _, err := s.PutChunks(ctx, repo, map[domain.ContentHash][]byte{hash: plan.Bodies[hash]}); err != nil {
			t.Fatal(err)
		}
		requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
	})
}

func TestPGStoredDocChunkProofRequiresChunksOwnedByRequestedRepo(t *testing.T) {
	s, ctx, repo, doc, plan := storedDocChunkFixturePG(t)
	requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
	other := domain.HashContent([]byte(string(repo) + " document-only owner"))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for hash := range plan.Bodies {
			if _, err := s.pool.Exec(cleanup, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, other, hash); err != nil {
				t.Error("restore second fixture owner's chunk grants", err)
			}
		}
	})
	// A valid document grant and globally present chunk bodies cannot borrow
	// another repository's chunk ownership or its cached verification proof.
	if _, err := s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, other, doc.Hash()); err != nil {
		t.Fatal(err)
	}
	proof, err := s.VerifyStoredDoc(ctx, other, doc.Hash())
	if !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
		t.Fatal("foreign chunk grants authorized the document", proof, err)
	}
	if _, _, err := s.PutChunks(ctx, other, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	requireStoredDocProofPG(t, s, ctx, other, doc.Hash())
	// Once both repositories are warm, revoking one owner's nonfirst chunk
	// must not affect the other owner or be masked by its surviving grant.
	hash := plan.Order[len(plan.Order)-1]
	if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, other, hash); err != nil {
		t.Fatal(err)
	}
	proof, err = s.VerifyStoredDoc(ctx, other, doc.Hash())
	if !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
		t.Fatal("warm foreign proof hid revoked chunk ownership", proof, err)
	}
	requireStoredDocProofPG(t, s, ctx, repo, doc.Hash())
}
