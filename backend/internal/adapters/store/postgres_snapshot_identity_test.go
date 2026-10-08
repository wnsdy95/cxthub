//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A root blob/grant is installed by fixture SQL only. The real document writer
// must remain closed; metadata persistence is not a publication proof.
func snapshotIdentityMetadataPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, identity domain.DocumentIdentity, text string) domain.Snapshot {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text + string(repo)}}}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	if identity == domain.DocumentIdentityLegacy {
		if _, err := s.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
			t.Fatal(err)
		}
	} else {
		manifest, _, err := domain.ConversationManifestForCIR(cir)
		if err != nil {
			t.Fatal(err)
		}
		hash, err = domain.ConversationManifestHash(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, Identity: identity, CIR: cir}); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
			t.Fatal("root document writer must remain disabled", err)
		}
		var granted bool
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2)`, string(repo), string(hash)).Scan(&granted); err != nil || granted {
			t.Fatal("rejected root granted ownership", granted, err)
		}
		raw, err = domain.CanonicalConversationManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		catalogExecPG(t, ctx, s.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), raw)
		catalogExecPG(t, ctx, s.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, string(repo), string(hash))
	}
	snap := domain.Snapshot{ID: hash, DocHash: hash, DocIdentity: identity, RepoID: repo, Branch: domain.StashBranchLabel, Message: "stash", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestPGSnapshotDocumentIdentityMetadataAndCatalog(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initial := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1})
	legacy := snapshotIdentityMetadataPG(t, ctx, s, repo, domain.DocumentIdentityLegacy, "legacy")
	root := snapshotIdentityMetadataPG(t, ctx, s, repo, domain.DocumentIdentityRootV1, "root")
	for _, snap := range []domain.Snapshot{legacy, root} {
		got, err := s.GetSnapshot(ctx, repo, snap.ID)
		if err != nil || got.DocIdentity != snap.DocIdentity {
			t.Fatal("identity read", got.DocIdentity, err)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"doc_identity"`)) != (snap.DocIdentity != "") {
			t.Fatal("legacy omission/root identity wire", string(raw))
		}
	}
	all, err := s.ListSnapshots(ctx, repo, "")
	if err != nil || len(all) != 2 {
		t.Fatal("list identities", len(all), err)
	}
	found := map[domain.ContentHash]domain.DocumentIdentity{}
	for _, snap := range all {
		found[snap.ID] = snap.DocIdentity
	}
	if found[legacy.ID] != "" || found[root.ID] != domain.DocumentIdentityRootV1 {
		t.Fatal("list dropped discriminator")
	}
	delta, cp := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: initial.Checkpoint, Limit: 1}), 1)
	if len(delta) != 2 {
		t.Fatal("unexpected metadata delta", len(delta))
	}
	for _, entry := range delta {
		got := catalogJSONPG[domain.Snapshot](t, entry.Value)
		if got.DocIdentity != found[got.ID] || bytes.Contains(entry.Value, []byte(`"doc_identity"`)) != (got.ID == root.ID) {
			t.Fatal("catalog discriminator/omission mismatch")
		}
	}
	beforeRoot := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	catalogMerkleBaselinePG(t, ctx, s, repo, beforeRoot)
	for _, snap := range []domain.Snapshot{legacy, root} {
		before := catalogReadStatePG(t, ctx, s, repo)
		changed := snap
		changed.Branch = "main"
		changed.Message = "must not promote"
		if changed.DocIdentity == "" {
			changed.DocIdentity = domain.DocumentIdentityRootV1
		} else {
			changed.DocIdentity = ""
		}
		if err := s.PutSnapshot(ctx, changed); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("identity relabel did not conflict", err)
		}
		got, err := s.GetSnapshot(ctx, repo, snap.ID)
		if err != nil || got.DocIdentity != snap.DocIdentity || got.Branch != snap.Branch || got.Message != snap.Message {
			t.Fatal("conflict changed snapshot", got, err)
		}
		if after := catalogReadStatePG(t, ctx, s, repo); after != before {
			t.Fatal("conflict changed catalog checkpoint")
		}
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal("same identity retry", err)
		}
	}
	unchanged := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &cp})
	if len(unchanged.Entries) != 0 {
		t.Fatal("identity replay fabricated delta")
	}
	root.Branch = "main"
	root.Message = "promoted"
	if err := s.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}
	promoted, err := s.GetSnapshot(ctx, repo, root.ID)
	if err != nil || promoted.DocIdentity != root.DocIdentity || promoted.Message != "promoted" {
		t.Fatal("same identity promotion failed", err)
	}
	afterRoot := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	catalogMerkleBaselinePG(t, ctx, s, repo, afterRoot)
	if afterRoot.RootHash == beforeRoot.RootHash {
		t.Fatal("catalog did not propagate promotion")
	}
	frozen := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1, RootHash: beforeRoot.RootHash, Checkpoint: &beforeRoot.Checkpoint})
	if !reflect.DeepEqual(frozen, beforeRoot) {
		t.Fatal("metadata update changed frozen Merkle root")
	}
	foreign := domain.HashContent([]byte("unowned identity scope"))
	if _, err := s.GetSnapshot(ctx, foreign, root.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign metadata visible", err)
	}
	for _, statement := range []string{
		`UPDATE snapshots SET doc_identity='cxt-manifest-sha256-v1' WHERE repo_id=$1 AND id=$2`,
		`UPDATE snapshots SET doc_identity='unknown' WHERE repo_id=$1 AND id=$2`,
	} {
		_, err := s.pool.Exec(ctx, statement, string(repo), string(legacy.ID))
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatal("SQL identity mutation accepted", err)
		}
	}
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET doc_identity=doc_identity WHERE repo_id=$1`, string(repo))
}

func TestPGSnapshotDocumentIdentityConcurrentRelabel(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	snap := snapshotIdentityMetadataPG(t, ctx, s, repo, domain.DocumentIdentityLegacy, "concurrent")
	// Remove only synthetic metadata; ownership stays. Race both identity claims
	// through the real insert/conflict path and require exactly one winner.
	catalogExecPG(t, ctx, s.pool, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, string(repo), string(snap.ID))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, identity := range []domain.DocumentIdentity{"", domain.DocumentIdentityRootV1} {
		next := snap
		next.DocIdentity = identity
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- s.PutSnapshot(ctx, next) }()
	}
	close(start)
	wg.Wait()
	close(results)
	passed, conflicts := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, domain.ErrIntegrity) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicts != 1 {
		t.Fatal("concurrent identity winners", passed, conflicts)
	}
}

func TestPGSnapshotDocumentIdentityUpgradePreservesLegacy(t *testing.T) {
	ctx := t.Context()
	s, prior := catalogUpgradeStore(t, ctx)
	// Start with an actual pre-0076 row and a retained catalog/Merkle checkpoint.
	repo := domain.HashContent([]byte("identity upgrade repo"))
	id := domain.HashContent([]byte("identity upgrade snapshot"))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, s.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(id), []byte(`{}`))
	catalogExecPG(t, ctx, s.pool, `INSERT INTO snapshots(repo_id,id,branch,doc_hash,parents,provider,fidelity) VALUES($1,$2,'main',$2,'{}','codex','full')`, string(repo), string(id))
	copyMigration := func(dir, name string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyMigration(prior, "0075_catalog_merkle.sql")
	if _, err := s.ApplyMigrations(ctx, prior); err != nil {
		t.Fatal(err)
	}
	contents := catalogUpgradeContents(t, ctx, s)
	before := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	entries, checkpoint := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1}), 1)
	legacy := catalogJSONPG[domain.Snapshot](t, entries[1].Value)
	fingerprint, err := domain.SnapshotStateHash(legacy)
	if err != nil {
		t.Fatal(err)
	}
	upgrade := t.TempDir()
	copyMigration(upgrade, "0076_snapshot_doc_identity.sql")
	if n, err := s.ApplyMigrations(ctx, upgrade); err != nil || n != 1 {
		t.Fatal("identity upgrade", n, err)
	}
	if got := catalogUpgradeContents(t, ctx, s); got != contents {
		t.Fatal("migration rewrote legacy catalog state/images")
	}
	got, err := s.GetSnapshot(ctx, repo, id)
	if err != nil || got.DocIdentity != "" {
		t.Fatal("legacy default", got.DocIdentity, err)
	}
	if hash, err := domain.SnapshotStateHash(got); err != nil || hash != fingerprint {
		t.Fatal("legacy fingerprint drift", hash, err)
	}
	var image []byte
	if err := s.pool.QueryRow(ctx, `SELECT cxt_catalog_image('snapshot',to_jsonb(s)) FROM snapshots s WHERE repo_id=$1 AND id=$2`, string(repo), string(id)).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(image, entries[1].Value) {
		t.Fatal("new image function changed legacy encoding")
	}
	warm := catalogMerklePagePG(t, ctx, s, repo, domain.CatalogMerkleRequest{Version: 1})
	if !reflect.DeepEqual(warm, before) {
		t.Fatal("migration invalidated legacy Merkle root/checkpoint")
	}
	delta := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &checkpoint})
	if len(delta.Entries) != 0 {
		t.Fatal("migration emitted legacy metadata delta")
	}
	if n, err := s.ApplyMigrations(ctx, upgrade); err != nil || n != 0 {
		t.Fatal("upgrade replay", n, err)
	}
}
