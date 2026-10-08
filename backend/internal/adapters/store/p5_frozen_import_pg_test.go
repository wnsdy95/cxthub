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

// A separately supplied empty target keeps migration tests away from ordinary
// PG fixtures. The harness provisions a private schema in its owned database.
func TestP5FrozenImportAtomicRootGraph(t *testing.T) {
	dsn := os.Getenv("CXT_P5_IMPORT_DSN")
	if dsn == "" {
		t.Skip("CXT_P5_IMPORT_DSN unset")
	}
	ctx := context.Background()
	pg, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if _, err := pg.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	if err := pg.CheckImportTargetEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	source := NewFSStore(t.TempDir())
	user := domain.User{ID: "p5-import-user", Username: "p5-import-user", Name: "P5", CreatedAt: time.Now().UTC()}
	if err := source.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "P5", Slug: "p5", OwnerID: user.ID, OwnerUsername: user.Username, CreatedAt: time.Now().UTC()}
	if err := source.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := source.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", RepositoryID: repository.ID}); err != nil {
		t.Fatal(err)
	}
	if err := source.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	_, legacy, _ := docJobFixture(t, repo, "legacy control")
	if _, err := source.PutVerifiedDoc(ctx, repo, legacy); err != nil {
		t.Fatal(err)
	}
	old := domain.Snapshot{ID: legacy.Hash(), DocHash: legacy.Hash(), RepoID: repo, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := source.PutSnapshot(ctx, old); err != nil {
		t.Fatal(err)
	}
	root := rootFixture(t, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
	seedRootFS(t, source, repo, root)
	snap := domain.Snapshot{ID: root.hash, DocHash: root.hash, DocIdentity: domain.DocumentIdentityRootV1, RepoID: repo, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Parents: []domain.ContentHash{old.ID}, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := source.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := source.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}, ""); err != nil {
		t.Fatal(err)
	}
	complete := p5RootJob(t, repo, root)
	complete.State = "completed"
	if err := source.writeDocJob(complete); err != nil {
		t.Fatal(err)
	}
	empty := rootFixture(t, "")
	seedRootFS(t, source, repo, empty)
	emptyJob := p5RootJob(t, repo, empty)
	if err := source.writeDocJob(emptyJob); err != nil {
		t.Fatal(err)
	}
	pending := rootFixture(t, "pending root unique dependency")
	for h, body := range pending.bodies {
		if err := writeAtomic(source.chunkPath(repo, h), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	running := p5RootJob(t, repo, pending).Claim(time.Now().UTC(), time.Hour)
	if err := source.writeDocJob(running); err != nil {
		t.Fatal(err)
	}
	assertEmpty := func() {
		t.Helper()
		for _, table := range []string{"users", "repos", "repo_blobs", "snapshots", "refs", "doc_finalization_jobs", "storage_usage_objects", "storage_usage_ledger"} {
			var n int
			if err := pg.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rollback table %s count=%d err=%v", table, n, err)
			}
		}
	}
	report, err := pg.ImportFrozenFS(ctx, source.dataDir, false)
	if err != nil || report.Applied {
		t.Fatal("valid dry run", report, err)
	}
	assertEmpty()
	// Each mutation occurs in the frozen source, never in a real replica. Restore
	// it before the next case; failed imports must roll back all earlier writes.
	for _, mode := range []string{"wrong_snapshot_tag", "corrupt_root_chunk", "missing_job_chunk", "malformed_descriptor", "malformed_job", "missing_requirement", "unknown_requirement"} {
		t.Run(mode, func(t *testing.T) {
			var path string
			var change []byte
			remove := false
			switch mode {
			case "missing_requirement", "unknown_requirement":
				path = source.repoDir(repo) + "/repo.json"
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var bad map[string]json.RawMessage
				if err := json.Unmarshal(raw, &bad); err != nil {
					t.Fatal(err)
				}
				bad["required_doc_identity"] = json.RawMessage(`""`)
				if mode == "unknown_requirement" {
					bad["required_doc_identity"] = json.RawMessage(`"future-root"`)
				}
				change, err = json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(change) {
					t.Fatal("requirement fixture is not valid JSON")
				}
			case "wrong_snapshot_tag":
				path = source.snapshotPath(repo, snap.ID)
				bad := snap
				bad.DocIdentity = domain.DocumentIdentityLegacy
				var err error
				change, err = json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
			case "corrupt_root_chunk":
				path = source.chunkPath(repo, root.manifest.Chunks[0].Hash)
				change = []byte("broken")
			case "missing_job_chunk":
				path = source.chunkPath(repo, pending.manifest.Chunks[0].Hash)
				remove = true
			case "malformed_descriptor":
				path = source.docPath(repo, root.hash)
				change = []byte(`{"identity":null}`)
			case "malformed_job":
				path = source.docJobPath(repo, running.ID)
				// Bypass the producer's validating MarshalJSON intentionally: a
				// durable corrupt job must remain syntactically valid JSON so this
				// case reaches the importer's representation validator.
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var bad map[string]json.RawMessage
				if err := json.Unmarshal(raw, &bad); err != nil {
					t.Fatal(err)
				}
				bad["root_manifest"] = json.RawMessage(`{"identity":null}`)
				change, err = json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(change) {
					t.Fatal("job fixture is not valid JSON")
				}
				var decoded domain.DocFinalizationJob
				if err := json.Unmarshal(change, &decoded); err != nil {
					t.Fatal("job fixture fails JSON decoding", err)
				}
				if err := decoded.Validate(); !errors.Is(err, domain.ErrConversationManifest) {
					t.Fatal("job fixture misses representation validation", err)
				}
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if remove {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, change, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = pg.ImportFrozenFS(ctx, source.dataDir, true)
			if restore := writeAtomic(path, original); restore != nil {
				t.Fatal(restore)
			}
			if err == nil {
				t.Fatal("invalid source imported")
			}
			if mode == "malformed_job" && !errors.Is(err, domain.ErrConversationManifest) {
				t.Fatal("malformed job did not reach representation rejection", err)
			}
			if mode == "unknown_requirement" && !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("unknown requirement did not reach identity rejection", err)
			}
			assertEmpty()
		})
	}
	// Existing deferred quota failures occur at commit. An injected constraint
	// exercises that rollback boundary without allocating gigabytes of fixtures.
	if _, err := pg.pool.Exec(ctx, `CREATE FUNCTION p5_import_quota() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic quota rejection' USING ERRCODE='CXT01'; END $$;
 CREATE CONSTRAINT TRIGGER p5_import_quota AFTER INSERT ON repo_blobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.kind='doc') EXECUTE FUNCTION p5_import_quota()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ImportFrozenFS(ctx, source.dataDir, true); err == nil {
		t.Fatal("deferred quota accepted")
	}
	assertEmpty()
	if _, err := pg.pool.Exec(ctx, `DROP TRIGGER p5_import_quota ON repo_blobs;DROP FUNCTION p5_import_quota()`); err != nil {
		t.Fatal(err)
	}
	before := rootFSImage(t, source.dataDir)
	report, err = pg.ImportFrozenFS(ctx, source.dataDir, true)
	if err != nil || !report.Applied {
		t.Fatal(report, err)
	}
	after := rootFSImage(t, source.dataDir)
	if len(before) != len(after) {
		t.Fatal("import mutated source")
	}
	for path, v := range before {
		if after[path] != v {
			t.Fatal("import mutated source")
		}
	}
	importedRepo, err := pg.GetRepo(ctx, repo)
	if err != nil || importedRepo.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal("repository requirement changed", importedRepo, err)
	}
	raw, err := pg.readOwnedDocObject(ctx, repo, "doc", root.hash)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source.docPath(repo, root.hash))
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("exact root bytes changed", err)
	}
	got, err := pg.GetSnapshot(ctx, repo, snap.ID)
	if err != nil || got.DocumentRef() != snap.DocumentRef() {
		t.Fatal("snapshot identity changed", err)
	}
	proof, err := pg.ReadVerifiedDoc(ctx, repo, root.hash)
	if err != nil || proof.DocumentRef() != snap.DocumentRef() {
		t.Fatal("imported root incomplete", err)
	}
	if _, err := pg.GetDoc(ctx, repo, legacy.Hash()); err != nil {
		t.Fatal("legacy import regressed", err)
	}
	for _, j := range []domain.DocFinalizationJob{complete, emptyJob, running} {
		got, err := pg.GetDocJob(ctx, repo, j.ID)
		if err != nil || got.DocumentRef() != j.DocumentRef() {
			t.Fatal(got, err)
		}
		if j.State == "running" {
			if got.State != "retrying" || got.Version != j.Version+1 || got.Fences(j, time.Now()) {
				t.Fatal("imported stale worker remains authorized")
			}
		} else if got.State != j.State {
			t.Fatal("receipt state changed")
		}
	}
	// The frozen importer deliberately supports the existing personal identity
	// dataset, not namespace/organization migration. Attach its imported personal
	// repository through the existing ownership path to exercise metering.
	ns := domain.NewID("ns_")
	if err := pg.CreateNamespace(ctx, domain.Namespace{ID: ns, Slug: user.Username, Kind: domain.NamespaceUser, UserID: user.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE repositories SET owner_namespace_id=$2 WHERE id=$1`, repository.ID, ns); err != nil {
		t.Fatal(err)
	}
	var want int64
	if err := pg.pool.QueryRow(ctx, `SELECT sum(octet_length(b.bytes)) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1`, repo).Scan(&want); err != nil {
		t.Fatal(err)
	}
	usage, err := pg.ReadStorageUsage(ctx, ns, time.Now().Add(-time.Hour), time.Now())
	if err != nil || usage.CurrentBytes != want {
		t.Fatalf("unique ownership accounting got=%d want=%d err=%v", usage.CurrentBytes, want, err)
	}
	if err := pg.ReconcileStorageUsage(ctx, ns); err != nil {
		t.Fatal(err)
	}
	usage, err = pg.ReadStorageUsage(ctx, ns, time.Now().Add(-time.Hour), time.Now())
	if err != nil || usage.CurrentBytes != want {
		t.Fatal("reconciliation changed accounting", err)
	}
	if err := pg.DeleteDoc(ctx, repo, root.hash); err != nil {
		t.Fatal(err)
	}
	for h := range root.bodies {
		if _, err := pg.readOwnedDocObject(ctx, repo, "chunk", h); err != nil {
			t.Fatal("document deletion added chunk GC", err)
		}
	}
}
