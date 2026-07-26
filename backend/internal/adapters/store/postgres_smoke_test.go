//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestPGSmoke is a smoke test for actual Postgres — runs only when CXT_TEST_DSN is set.
// Skipped when unset (local `go test` skips, CI injects service container DSN).
//
// Validation: idempotency of migrations → fixes paths in code from PG re-hypothetical runs up to memory(nil slice).
func TestPGSmoke(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset — PG smoke test skipped")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Migration idempotency: 1st application (N>0) → 2nd application (0).
	n1, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n1 == 0 {
		t.Fatalf("0 migrations applied — check migration path")
	}
	n2, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations")
	if err != nil || n2 != 0 {
		t.Fatalf("idempotency violation: 2nd application n=%d err=%v", n2, err)
	}

	// User, workspace, member(5 role) CHECK passed — 0016.
	uid := "dev:pgsmoke@t.io"
	if err := st.UpsertUser(ctx, domain.User{ID: uid, Email: "pgsmoke@t.io", Name: "PG", Username: "pgsmoke"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	ws := domain.Workspace{ID: domain.NewID("ws_"), Name: "PGSmoke", OwnerID: uid, Slug: "pgsmoke", OwnerUsername: "pgsmoke", CreatedAt: time.Now().UTC()}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	for _, role := range []domain.MemberRole{domain.RoleViewer, domain.RolePuller, domain.RoleMaintainer} {
		muid := uid + ":" + string(role)
		// Create real users first to satisfy membership FK (user_id → users.id).
		if err := st.UpsertUser(ctx, domain.User{ID: muid, Email: string(role) + "@t.io", Name: string(role), Username: "pgsmoke-" + string(role)}); err != nil {
			t.Fatalf("member user(%s): %v", role, err)
		}
		if err := st.AddMember(ctx, domain.Membership{WorkspaceID: ws.ID, UserID: muid, Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("member(%s): %v", role, err)
		}
	}

	// Session (hash storage — hint/kind columns, 0015).
	sess := domain.Session{Token: domain.HashToken("sess_x"), UserID: uid, Hint: "xxxxxxxx", Kind: "web", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	if got, err := st.GetSession(ctx, sess.Token); err != nil || got.Kind != "web" {
		t.Fatalf("get session: kind=%q err=%v", got.Kind, err)
	}

	// Snapshot → ref(FK) → memory(nil slice → NOT NULL normalization, rehash discovery).
	repoID := domain.HashContent([]byte("pgsmoke-repo"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repoID, WorkspaceID: ws.ID, GitRemoteURL: "https://github.com/acme/shared.git", DefaultBranch: "main"}); err != nil {
		t.Fatalf("repo: %v", err)
	}
	// Bounded chunk staging: store/mutual exclusion dedup/repo ownership isolation verified by actual PG transaction.
	chunkBody := []byte("pg smoke bounded chunk")
	chunkHash := domain.HashContent(chunkBody)
	stored, deduped, err := st.PutChunks(ctx, repoID, map[domain.ContentHash][]byte{chunkHash: chunkBody})
	if err != nil || stored != 1 || deduped != 0 {
		t.Fatalf("stage chunk: stored=%d deduped=%d err=%v", stored, deduped, err)
	}
	stored, deduped, err = st.PutChunks(ctx, repoID, map[domain.ContentHash][]byte{chunkHash: chunkBody})
	if err != nil || stored != 0 || deduped != 1 {
		t.Fatalf("dedup chunk: stored=%d deduped=%d err=%v", stored, deduped, err)
	}
	if got, err := st.GetChunk(ctx, repoID, chunkHash); err != nil || string(got) != string(chunkBody) {
		t.Fatalf("get chunk: body=%q err=%v", got, err)
	}
	// Store doc first (snapshot doc_hash FK → blobs). Same order as actual push flow.
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"}}
	cb, _ := domain.CanonicalBytes(cir)
	snapID := domain.HashContent(cb)
	if _, err := st.PutDoc(ctx, repoID, domain.SessionDoc{Hash: snapID, CIR: cir}); err != nil {
		t.Fatalf("doc: %v", err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapID, RepoID: repoID, Branch: "main", DocHash: snapID, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Message: "smoke"}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: "main", Target: snapID}
	if err := st.CompareAndSwapRef(ctx, repoID, ref, ""); err != nil {
		t.Fatalf("ref(FK): %v", err)
	}
	if err := st.CompareAndSwapRef(ctx, repoID, domain.Ref{RepoID: repoID, Kind: domain.RefHead, Name: domain.HeadRefName, Symbolic: "main"}, ""); err != nil {
		t.Fatalf("symbolic HEAD(NULL target): %v", err)
	}
	// Set KeyFacts/OpenTasks to nil to take NOT NULL normalization path.
	digest := domain.MemoryDigest{SnapshotID: snapID, Summary: "m", Provider: domain.ProviderClaude}
	memoryHash, err := st.PutMemory(ctx, repoID, digest)
	if err != nil {
		t.Fatalf("memory blob: %v", err)
	}
	if err := st.PutMemoryMeta(ctx, repoID, digest); err != nil {
		t.Fatalf("memory(nil slice): %v", err)
	}
	if err := st.SetSnapshotMemory(ctx, repoID, snapID, memoryHash); err != nil {
		t.Fatalf("attach memory: %v", err)
	}

	// Same Git URL and content hash can coexist in different workspace repos. However, before repo supplies directly, a global CAS blob can only be read by hash.
	ws2 := domain.Workspace{ID: domain.NewID("ws_"), Name: "PGSmoke2", OwnerID: uid, Slug: "pgsmoke2", OwnerUsername: "pgsmoke", CreatedAt: time.Now().UTC()}
	if err := st.CreateWorkspace(ctx, ws2); err != nil {
		t.Fatalf("workspace2: %v", err)
	}
	repo2 := domain.HashContent([]byte("pgsmoke-repo-2"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo2, WorkspaceID: ws2.ID, GitRemoteURL: "https://github.com/acme/shared.git", DefaultBranch: "main"}); err != nil {
		t.Fatalf("repo2: %v", err)
	}
	if _, err := st.GetDoc(ctx, repo2, snapID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-repo doc lookup = %v, want ErrNotFound", err)
	}
	if _, err := st.GetMemory(ctx, repo2, memoryHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-repo memory lookup = %v, want ErrNotFound", err)
	}
	if _, err := st.GetChunk(ctx, repo2, chunkHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-repo chunk lookup = %v, want ErrNotFound", err)
	}
	if stored, deduped, err := st.PutChunks(ctx, repo2, map[domain.ContentHash][]byte{chunkHash: chunkBody}); err != nil || stored != 1 || deduped != 0 {
		t.Fatalf("repo2 chunk ownership: stored=%d deduped=%d err=%v", stored, deduped, err)
	}
	if _, err := st.PutDoc(ctx, repo2, domain.SessionDoc{Hash: snapID, CIR: cir}); err != nil {
		t.Fatalf("repo2 doc dedup: %v", err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapID, RepoID: repo2, Branch: "main", DocHash: snapID, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Message: "same hash, separate repo"}); err != nil {
		t.Fatalf("repo2 snapshot with same id: %v", err)
	}
	if got, err := st.GetSnapshot(ctx, repo2, snapID); err != nil || got.RepoID != repo2 {
		t.Fatalf("repo2 snapshot: repo=%s err=%v", got.RepoID, err)
	}
}
