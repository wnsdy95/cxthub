//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rootProjectionCounts(t *testing.T, s *PostgresStore, ctx context.Context, hash domain.ContentHash) [3]int {
	t.Helper()
	var counts [3]int
	for i, q := range []string{
		`SELECT count(*) FROM doc_read_indexes_v3 WHERE hash=$1`,
		`SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=$1`,
		`SELECT count(*) FROM doc_read_block_preparations_v3 p JOIN doc_finalization_jobs j ON j.repo_id=p.repo_id AND j.id=p.job_id WHERE convert_from(j.payload,'UTF8')::jsonb->>'doc_hash'=$1`,
	} {
		if err := s.pool.QueryRow(ctx, q, hash).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func rootProjectionNoIndex(t *testing.T, s *PostgresStore, ctx context.Context, hash domain.ContentHash) {
	t.Helper()
	if got := rootProjectionCounts(t, s, ctx, hash); got != [3]int{} {
		t.Fatalf("root persisted unused legacy projection/pins: %v", got)
	}
}

func rootProjectionSearchCount(t *testing.T, s *PostgresStore, ctx context.Context, doc domain.VerifiedSessionDoc) int {
	t.Helper()
	p, err := doc.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=ANY($1::text[])`, p.EventHashes()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRootProjectionPGDoesNotPublishLegacyIndex(t *testing.T) {
	s, ctx := rootProjectionIsolatedPG(t)
	ctx = rootPublicationContext(ctx)
	f := uniqueRootPublicationFixturePG(t, strings.Repeat("root literal 50%_\\ \ud55c\n", 40000))
	j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
	publication, err := s.PrepareDocJob(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = publication.Complete(ctx, j, time.Now()); err != nil {
		t.Fatal(err)
	}
	rootProjectionNoIndex(t, s, ctx, doc.Hash())
	if n := rootProjectionSearchCount(t, s, ctx, doc); n != 0 {
		t.Fatalf("root created %d search rows", n)
	}
	done, err := s.GetDocJob(ctx, j.RepoID, j.ID)
	if err != nil || done.State != "completed" {
		t.Fatal(done, err)
	}
	got, err := s.ReadVerifiedDoc(ctx, j.RepoID, doc.Hash())
	if err != nil || got.DocumentRef() != doc.DocumentRef() {
		t.Fatal(err)
	}
	// No snapshot is needed for maintenance. Root identification comes from bytes.
	if err = s.BackfillReadIndexes(ctx, func(int) {}); err != nil {
		t.Fatal(err)
	}
	rootProjectionNoIndex(t, s, ctx, doc.Hash())
	// A wrong legacy snapshot tag must not cause maintenance to rewrite a root.
	if err = s.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash(), RepoID: j.RepoID, DocHash: doc.Hash(), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err = s.BackfillReadIndexes(ctx, func(int) {}); err != nil {
		t.Fatal(err)
	}
	rootProjectionNoIndex(t, s, ctx, doc.Hash())
	if err = s.DeleteSnapshot(ctx, j.RepoID, doc.Hash()); err != nil {
		t.Fatal(err)
	}
	// Publishing the same canonical events under legacy identity still creates
	// exactly the original indexed projection and remains searchable after root GC.
	legacy, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(f.canonical), CIR: f.cir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutVerifiedDoc(ctx, j.RepoID, legacy); err != nil {
		t.Fatal(err)
	}
	if n := rootProjectionSearchCount(t, s, ctx, doc); n != len(f.cir.Events) {
		t.Fatalf("legacy search rows=%d", n)
	}
	hits, err := s.SearchDocEvents(ctx, j.RepoID, legacy.Hash(), "50%_\\", -1, 10)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	if err = s.DeleteDoc(ctx, j.RepoID, doc.Hash()); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchDocEvents(ctx, j.RepoID, legacy.Hash(), "50%_\\", -1, 10)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	for h := range f.bodies {
		if _, err = s.GetChunk(ctx, j.RepoID, h); err != nil {
			t.Fatal("root deletion lost chunk grant", err)
		}
	}
}

// Recreate the previous writer's actual committed stage, not a new empty plan.
// This helper deliberately uses existing production insertion and retention SQL.
func rootProjectionOldStage(t *testing.T, s *PostgresStore, ctx context.Context, j domain.DocFinalizationJob, doc domain.VerifiedSessionDoc) {
	t.Helper()
	p, err := prepareReadIndexPG(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var payload []byte
	if err = tx.QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err = putPreparedReadBlocksPG(ctx, tx, p.blocks); err != nil {
		t.Fatal(err)
	}
	hashes := make([]domain.ContentHash, len(p.blocks))
	for i, b := range p.blocks {
		hashes[i] = b.Hash
	}
	if _, err = tx.Exec(ctx, `INSERT INTO doc_read_block_preparations_v3(repo_id,job_id,version,block_hash) SELECT $1,$2,$3,h FROM unnest($4::text[]) AS x(h) ORDER BY h`, j.RepoID, j.ID, j.Version, hashes); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRootProjectionPGOldStageRecovery(t *testing.T) {
	for _, mode := range []string{"complete", "shared-legacy", "rollback", "reclaim", "expired", "retry", "reject"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := rootProjectionIsolatedPG(t)
			ctx = rootPublicationContext(ctx)
			f := uniqueRootPublicationFixturePG(t, "old staged root "+t.Name())
			j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
			rootProjectionOldStage(t, s, ctx, j, doc)
			if rootProjectionSearchCount(t, s, ctx, doc) == 0 {
				t.Fatal("old stage fixture has no text")
			}
			var legacy domain.VerifiedSessionDoc
			if mode == "shared-legacy" {
				var err error
				legacy, err = domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(f.canonical), CIR: f.cir})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.PutVerifiedDoc(ctx, j.RepoID, legacy); err != nil {
					t.Fatal(err)
				}
			}
			pub, err := s.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "rollback":
				before := rootPGImage(t, ctx, s, j.RepoID, "")
				pinsBefore := rootProjectionCounts(t, s, ctx, doc.Hash())
				stop := errors.New("abort outer root publication")
				err = s.WithinRepository(ctx, j.RepoID, func(bound context.Context) error {
					if err := pub.Complete(bound, j, time.Now()); err != nil {
						return err
					}
					// An independent observer sees neither new ownership nor a completed job.
					assertDocNotPublishedPG(t, s, ctx, j)
					observed, e := s.GetDocJob(ctx, j.RepoID, j.ID)
					if e != nil || observed.State != "running" {
						t.Fatal(observed, e)
					}
					return stop
				})
				if !errors.Is(err, stop) || rootPGImage(t, ctx, s, j.RepoID, "") != before || rootProjectionCounts(t, s, ctx, doc.Hash()) != pinsBefore {
					t.Fatal("outer rollback changed root/job/usage", err)
				}
			case "expired":
				expired := j
				expired.LeaseUntil = time.Now().Add(-time.Second)
				if err = s.writeDocJob(ctx, expired); err != nil {
					t.Fatal(err)
				}
				if err = pub.Complete(ctx, j, time.Now()); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("expired job", err)
				}
				assertDocNotPublishedPG(t, s, ctx, j)
			case "reclaim":
				next, e := s.ClaimDocJob(ctx, j.RepoID, time.Now().Add(2*time.Minute), time.Minute)
				if e != nil {
					t.Fatal(e)
				}
				if err = pub.Complete(ctx, j, time.Now()); !errors.Is(err, domain.ErrConflict) {
					t.Fatal("stale claim", err)
				}
				j = next
			case "retry", "reject":
				j.State = "retrying"
				if mode == "reject" {
					j.State = "rejected"
				}
				j.LeaseUntil = time.Time{}
				j.NextAttempt = time.Now().Add(time.Hour)
				if err = s.FinishDocJob(ctx, j, time.Now()); err != nil {
					t.Fatal(err)
				}
				finished, e := s.GetDocJob(ctx, j.RepoID, j.ID)
				if e != nil || finished.State != j.State {
					t.Fatal("retirement state", e)
				}
				if have, e := s.HasDocs(ctx, j.RepoID, []domain.ContentHash{doc.Hash()}); e != nil || len(have) != 0 {
					t.Fatal("retirement published root", have, e)
				}
				rootProjectionNoIndex(t, s, ctx, doc.Hash())
				if n := rootProjectionSearchCount(t, s, ctx, doc); n != 0 {
					t.Fatal("retired stage retained text", n)
				}
				return
			}
			if mode == "expired" {
				return
			}
			if err = pub.Complete(ctx, j, time.Now()); err != nil {
				t.Fatal(err)
			}
			rootProjectionNoIndex(t, s, ctx, doc.Hash())
			want := 0
			if mode == "shared-legacy" {
				want = len(f.cir.Events)
			}
			if n := rootProjectionSearchCount(t, s, ctx, doc); n != want {
				t.Fatalf("stage cleanup got=%d want=%d", n, want)
			}
			if mode == "shared-legacy" {
				if _, err = s.DocReadIndex(ctx, j.RepoID, legacy.Hash()); err != nil {
					t.Fatal(err)
				}
				if err = s.DeleteDoc(ctx, j.RepoID, legacy.Hash()); err != nil {
					t.Fatal(err)
				}
				if n := rootProjectionSearchCount(t, s, ctx, doc); n != 0 {
					t.Fatal("legacy last-owner cleanup", n)
				}
				if _, err = s.ReadVerifiedDoc(ctx, j.RepoID, doc.Hash()); err != nil {
					t.Fatal("root depended on deleted projection", err)
				}
			}
		})
	}
}

func TestRootProjectionPGBackfillFailsCurrentCorruption(t *testing.T) {
	s, ctx := rootProjectionIsolatedPG(t)
	ctx = rootPublicationContext(ctx)
	f := uniqueRootPublicationFixturePG(t, "backfill root corruption")
	j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
	if err := s.CompleteDocJob(ctx, j, doc, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := f.manifest.Chunks[0].Hash
	raw := docCompress(f.bodies[h])
	defer s.pool.Exec(context.Background(), `UPDATE blobs SET bytes=$2 WHERE hash=$1`, h, raw)
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, h, []byte("bad current bytes")); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillReadIndexes(ctx, func(int) {}); err == nil {
		t.Fatal("backfill accepted corrupt root")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, h, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillReadIndexes(ctx, func(int) {}); err != nil {
		t.Fatal(err)
	}
	rootProjectionNoIndex(t, s, ctx, f.hash)
	if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, j.RepoID, h); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillReadIndexes(ctx, func(int) {}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("backfill lost ownership fence", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, j.RepoID, h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.hash, []byte(`{"identity":"cxt-manifest-sha256-v1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillReadIndexes(ctx, func(int) {}); err == nil {
		t.Fatal("backfill accepted malformed root descriptor")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.hash, docCompress(f.manifestBytes)); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.BackfillReadIndexes(canceled, func(int) {}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRootProjectionPGConcurrentOldStages(t *testing.T) {
	s, ctx := rootProjectionIsolatedPG(t)
	ctx = rootPublicationContext(ctx)
	f := uniqueRootPublicationFixturePG(t, "shared old stage "+t.Name())
	j, doc := rootPublicationClaimPG(t, s, ctx, f, "")
	other := f
	other.cir.Envelope.SessionOriginID += "other"
	var err error
	other.manifest, other.bodies, err = domain.ConversationManifestForCIR(other.cir)
	if err != nil {
		t.Fatal(err)
	}
	other.hash, err = domain.ConversationManifestHash(other.manifest)
	if err != nil {
		t.Fatal(err)
	}
	other.manifestBytes, err = domain.CanonicalConversationManifest(other.manifest)
	if err != nil {
		t.Fatal(err)
	}
	j2, doc2 := rootPublicationClaimPG(t, s, ctx, other, "")
	rootProjectionOldStage(t, s, ctx, j, doc)
	rootProjectionOldStage(t, s, ctx, j2, doc2)
	p, _ := doc.PlanReadIndex()
	p2, _ := doc2.PlanReadIndex()
	if !reflect.DeepEqual(p.EventHashes(), p2.EventHashes()) {
		t.Fatal("fixture does not share exact events")
	}
	done := make(chan error, 2)
	for i, pair := range []struct {
		j   domain.DocFinalizationJob
		doc domain.VerifiedSessionDoc
	}{{j, doc}, {j2, doc2}} {
		t.Log(fmt.Sprintf("completion %d", i))
		go func() { done <- s.CompleteDocJob(ctx, pair.j, pair.doc, time.Now()) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range []domain.VerifiedSessionDoc{doc, doc2} {
		rootProjectionNoIndex(t, s, ctx, d.Hash())
		if _, err := s.ReadVerifiedDoc(ctx, []domain.ContentHash{j.RepoID, j2.RepoID}[i], d.Hash()); err != nil {
			t.Fatal(err)
		}
	}
	if n := rootProjectionSearchCount(t, s, ctx, doc); n != 0 {
		t.Fatal("concurrent retirement orphaned text", n)
	}
}

// Global worker/backfill APIs require a private database even when the package
// suite's CXT_TEST_DSN is shared. Never process or reset another test's queue.
func rootProjectionIsolatedDSN(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated CXT_TEST_DSN")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := domain.NewID("root_projection_")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Set("dbname", name)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " dbname=" + name
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != name {
		t.Fatal("isolated database selection failed", err)
	}
	t.Log("owned database", name)
	return dsn
}

func rootProjectionIsolatedPG(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Setenv("CXT_TEST_DSN", rootProjectionIsolatedDSN(t, setup))
	return chunkReusePG(t)
}
