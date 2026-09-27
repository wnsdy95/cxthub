//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func importTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	name := domain.NewID("cxt_import_")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+quoted)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	s := &PostgresStore{pool: pool}
	if _, err = s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPGFrozenImportAtomicityAndQueries(t *testing.T) {
	pg := importTestStore(t)
	ctx := context.Background()
	source := NewFSStore(t.TempDir())
	user := domain.User{ID: "migration-user", Username: "migration-user", Name: "Migration", CreatedAt: time.Now().UTC()}
	if err := source.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "Migration", Slug: "migration", OwnerID: user.ID, OwnerUsername: user.Username, CreatedAt: time.Now().UTC()}
	if err := source.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	member := domain.Membership{RepositoryID: repository.ID, UserID: user.ID, Role: domain.RolePuller, CreatedAt: time.Now().UTC()}
	if err := source.AddMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{"workspace_id": repository.ID, "user_id": user.ID, "role": "owner"}
	b, _ := json.Marshal(legacy)
	if err := writeAtomic(filepath.Join(source.membersDir(), repository.ID, "legacy.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(source.dataDir, "ownership-migration-v1.complete.json"), []byte(`{"version":"repository-ownership-v1"}`)); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := source.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", RepositoryID: repository.ID}); err != nil {
		t.Fatal(err)
	}
	alias := domain.RepositoryPathAlias{Owner: user.Username, Path: repository.Slug, RepositoryID: repository.ID, ContextRepoID: repo}
	b, _ = json.Marshal(alias)
	if err := writeAtomic(filepath.Join(source.dataDir, "repository-aliases", opaqueName("handle:"+user.Username+"/"+repository.Slug)+".json"), b); err != nil {
		t.Fatal(err)
	}
	job, doc, plan := docJobFixture(t, repo, "migration context")
	if _, err := source.PutVerifiedDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	mem := domain.MemoryDigest{SnapshotID: doc.Hash(), Summary: "retained memory", Provider: domain.ProviderCodex}
	mh, err := source.PutMemory(ctx, repo, mem)
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash(), RepoID: repo, DocHash: doc.Hash(), MemoryHash: mh, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err = source.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err = source.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = source.EnqueueDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = source.AdvanceEvidenceRevision(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err = source.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	report, err := pg.ImportFrozenFS(ctx, source.dataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied || report.Records["snapshots"] != 1 {
		t.Fatalf("report %+v", report)
	}
	var count int
	if err = pg.pool.QueryRow(ctx, `SELECT count(*) FROM repos`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry-run escaped rollback %d %v", count, err)
	}
	// Fail after identity/repository writes; every earlier write must roll back.
	chunk := source.chunkPath(repo, plan.Order[0])
	original, err := os.ReadFile(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(chunk, []byte("corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.ImportFrozenFS(ctx, source.dataDir, true); err == nil {
		t.Fatal("corrupt content accepted")
	}
	if err = pg.pool.QueryRow(ctx, `SELECT count(*) FROM repos`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import escaped rollback %d %v", count, err)
	}
	if err = os.WriteFile(chunk, original, 0600); err != nil {
		t.Fatal(err)
	}
	report, err = pg.ImportFrozenFS(ctx, source.dataDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied {
		t.Fatal("not applied")
	}
	var role string
	if err = pg.pool.QueryRow(ctx, `SELECT role FROM memberships WHERE repository_id=$1 AND user_id=$2`, repository.ID, user.ID).Scan(&role); err != nil || role != "puller" {
		t.Fatalf("legacy role resurrected: %s %v", role, err)
	}
	got, err := pg.GetSnapshot(ctx, repo, snap.ID)
	if err != nil || !got.CreatedAt.Equal(snap.CreatedAt) || got.MemoryHash != mh {
		t.Fatalf("snapshot changed %+v %v", got, err)
	}
	if got, err := pg.GetDoc(ctx, repo, snap.ID); err != nil || got.Hash != snap.ID {
		t.Fatalf("document lost %v", err)
	}
	if got, err := pg.GetMemory(ctx, repo, mh); err != nil || got.Summary != mem.Summary {
		t.Fatalf("memory lost %v", err)
	}
	if got, err := pg.GetDocJob(ctx, repo, job.ID); err != nil || got.State != job.State {
		t.Fatalf("queue state changed %v", err)
	}
	if got, err := pg.RepositoryRevision(ctx, repo); err != nil || got.Evidence != 1 || got.Graph != 1 {
		t.Fatalf("revision changed: %+v %v", got, err)
	}
	if err = pg.CheckImportTargetEmpty(ctx); err == nil {
		t.Fatal("schema guard accepted occupied target")
	}
	if _, err = pg.ImportFrozenFS(ctx, source.dataDir, true); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("occupied target allowed %v", err)
	}
}

func TestFrozenImportRejectsUnknownAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unknown.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectFrozenFS(dir); err == nil {
		t.Fatal("unknown source silently discarded")
	}
	if err := os.Remove(filepath.Join(dir, "unknown.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "users")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectFrozenFS(dir); err == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestFrozenDocStreamVerification(t *testing.T) {
	s := NewFSStore(t.TempDir())
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	_, doc, plan := docJobFixture(t, repo, strings.Repeat("test text", 100000))
	if _, _, err := s.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyFrozenDoc(ctx, s, repo, doc.Hash(), body); err != nil {
		t.Fatal(err)
	}
	if err = verifyFrozenDoc(ctx, s, repo, domain.HashContent([]byte("wrong")), body); err == nil {
		t.Fatal("wrong document identity accepted")
	}
	// The legacy event-per-line chunk format remains importable too.
	_, small, _ := docJobFixture(t, repo, "legacy text")
	legacy, ok := domain.PlanDocChunksV1(small.Bytes())
	if !ok {
		t.Fatal("legacy fixture did not chunk")
	}
	if _, _, err = s.PutChunks(ctx, repo, legacy.Bodies); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(legacy.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyFrozenDoc(ctx, s, repo, small.Hash(), body); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = verifyFrozenDoc(cancelled, s, repo, small.Hash(), body); err == nil {
		t.Fatal("cancellation ignored")
	}
}
