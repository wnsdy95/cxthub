//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
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

	// Large legacy memory migration keeps MemoryDigestHash stable while replacing
	// the global raw JSON with a component manifest. Every existing memory owner
	// must receive all memory_chunk grants before the shared representation flips.
	legacyMemory := domain.MemoryDigest{
		SnapshotID: snapID,
		Summary:    strings.Repeat("shared-memory-prefix-", 12<<10),
		KeyFacts:   []string{"identity remains the complete digest hash"},
		OpenTasks:  []string{},
		Provider:   domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{{
			SourceSnapshot: snapID,
			Summary:        strings.Repeat("memory-fragment-", 8<<10),
		}},
	}
	legacyMemoryHash, _ := domain.MemoryDigestHash(legacyMemory)
	legacyMemoryJSON, _ := json.Marshal(legacyMemory)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(legacyMemoryHash), legacyMemoryJSON); err != nil {
		t.Fatalf("legacy memory blob fixture: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'memory',$2)`, string(repoID), string(legacyMemoryHash)); err != nil {
		t.Fatalf("legacy memory owner fixture: %v", err)
	}
	if gotHash, err := st.PutMemory(ctx, repo2, legacyMemory); err != nil || gotHash != legacyMemoryHash {
		t.Fatalf("memory component migration: hash=%s err=%v", gotHash, err)
	}
	var storedMemory []byte
	if err := st.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(legacyMemoryHash)).Scan(&storedMemory); err != nil {
		t.Fatalf("read migrated memory manifest: %v", err)
	}
	storedMemory, err = docDecompress(storedMemory)
	if err != nil {
		t.Fatalf("decode migrated memory manifest: %v", err)
	}
	memoryManifest, isMemoryManifest, err := domain.ParseMemoryChunkManifest(storedMemory)
	if err != nil || !isMemoryManifest || memoryManifest.Format != domain.MemoryChunkFormatV1 {
		t.Fatalf("memory manifest=%+v is=%v err=%v", memoryManifest, isMemoryManifest, err)
	}
	allMemoryChunks := append(append([]domain.ContentHash{}, memoryManifest.SummaryChunks...), memoryManifest.FragmentChunks...)
	for _, owner := range []domain.ContentHash{repoID, repo2} {
		got, err := st.GetMemory(ctx, owner, legacyMemoryHash)
		if err != nil || got.Summary != legacyMemory.Summary {
			t.Fatalf("memory owner %s lost migrated body: %v", owner, err)
		}
		for _, chunkHash := range allMemoryChunks {
			var count int
			if err := st.pool.QueryRow(ctx,
				`SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2`,
				string(owner), string(chunkHash)).Scan(&count); err != nil || count != 1 {
				t.Fatalf("memory owner %s lacks component %s: count=%d err=%v", owner, chunkHash, count, err)
			}
		}
	}

	// Concurrent writers lock the top-level memory before components. Both a
	// legacy migrator and a deduplicating owner must finish without deadlock.
	raceMemory := legacyMemory
	raceMemory.Summary = strings.Repeat("memory-lock-order-", 10<<10)
	raceMemoryHash, _ := domain.MemoryDigestHash(raceMemory)
	raceMemoryJSON, _ := json.Marshal(raceMemory)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(raceMemoryHash), raceMemoryJSON); err != nil {
		t.Fatalf("memory lock-order blob fixture: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'memory',$2)`, string(repoID), string(raceMemoryHash)); err != nil {
		t.Fatalf("memory lock-order owner fixture: %v", err)
	}
	memoryRaceCtx, cancelMemoryRace := context.WithTimeout(ctx, 10*time.Second)
	defer cancelMemoryRace()
	startMemoryRace := make(chan struct{})
	memoryRaceErrs := make(chan error, 2)
	for _, owner := range []domain.ContentHash{repoID, repo2} {
		owner := owner
		go func() {
			<-startMemoryRace
			_, err := st.PutMemory(memoryRaceCtx, owner, raceMemory)
			memoryRaceErrs <- err
		}()
	}
	close(startMemoryRace)
	for i := 0; i < 2; i++ {
		if err := <-memoryRaceErrs; err != nil {
			t.Fatalf("concurrent memory/component lock ordering: %v", err)
		}
	}

	// A corrupt globally deduplicated memory component aborts the complete
	// transaction, including the newly inserted top-level memory object.
	corruptMemory := domain.MemoryDigest{
		SnapshotID: snapID,
		Summary:    strings.Repeat("corrupt-memory-component-", 8<<10),
		Provider:   domain.ProviderClaude,
	}
	corruptMemoryHash, _ := domain.MemoryDigestHash(corruptMemory)
	corruptMemoryPlan, ok, err := domain.PlanMemoryChunks(corruptMemory)
	if err != nil || !ok {
		t.Fatalf("corrupt memory plan: ok=%v err=%v", ok, err)
	}
	badMemoryChunk := corruptMemoryPlan.Order[0]
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(badMemoryChunk), docCompress([]byte("corrupt memory body"))); err != nil {
		t.Fatalf("corrupt memory component fixture: %v", err)
	}
	if _, err := st.PutMemory(ctx, repoID, corruptMemory); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupt component PutMemory err=%v, want ErrIntegrity", err)
	}
	var failedMemoryCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, string(corruptMemoryHash)).Scan(&failedMemoryCount); err != nil || failedMemoryCount != 0 {
		t.Fatalf("failed PutMemory left top-level blob: count=%d err=%v", failedMemoryCount, err)
	}

	// Rolling v1→v2 dedup: a v2 writer must migrate the global manifest and grant
	// every old/new doc owner the v2 chunks instead of retaining two representations.
	v1CIR := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("v1", 400<<10)}}}},
	}
	v1Canonical, _ := domain.CanonicalBytes(v1CIR)
	v1Hash := domain.HashContent(v1Canonical)
	v1Plan, ok := domain.PlanDocChunksV1(v1Canonical)
	if !ok {
		t.Fatal("v1 PG fixture plan unavailable")
	}
	for _, chunkHash := range v1Plan.Order {
		if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(chunkHash), docCompress(v1Plan.Bodies[chunkHash])); err != nil {
			t.Fatalf("v1 chunk fixture: %v", err)
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'chunk',$2)`, string(repoID), string(chunkHash)); err != nil {
			t.Fatalf("v1 chunk owner fixture: %v", err)
		}
	}
	v1Manifest, _ := json.Marshal(v1Plan.Manifest)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(v1Hash), docCompress(v1Manifest)); err != nil {
		t.Fatalf("v1 manifest fixture: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'doc',$2)`, string(repoID), string(v1Hash)); err != nil {
		t.Fatalf("v1 doc owner fixture: %v", err)
	}
	if _, err := st.PutDoc(ctx, repo2, domain.SessionDoc{Hash: v1Hash, CIR: v1CIR}); err != nil {
		t.Fatalf("v2 writer dedup against v1: %v", err)
	}
	var migratedRaw []byte
	if err := st.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(v1Hash)).Scan(&migratedRaw); err != nil {
		t.Fatalf("read migrated manifest: %v", err)
	}
	migratedRaw, err = docDecompress(migratedRaw)
	if err != nil {
		t.Fatalf("decompress migrated manifest: %v", err)
	}
	migrated, ok := domain.ParseDocChunkManifest(migratedRaw)
	if !ok || migrated.Format != domain.ChunkFormatV2 {
		t.Fatalf("stored representation was not migrated to v2: %+v", migrated)
	}
	for _, owner := range []domain.ContentHash{repoID, repo2} {
		if got, err := st.GetDoc(ctx, owner, v1Hash); err != nil || domain.ValidateSessionDocHash(got) != nil {
			t.Fatalf("owner %s cannot read migrated v2 representation: %v", owner, err)
		}
		for _, chunkHash := range migrated.Chunks {
			if _, err := st.GetChunk(ctx, owner, chunkHash); err != nil {
				t.Fatalf("owner %s lacks migrated chunk %s: %v", owner, chunkHash, err)
			}
		}
	}

	// Legacy global monolith repack must grant chunks to every repo that owns
	// the doc before replacing the shared blob with a v2 manifest.
	legacyCIR := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", domain.MaxPortableChunkBytes+1)}}}},
	}
	legacyCanonical, _ := domain.CanonicalBytes(legacyCIR)
	legacyHash := domain.HashContent(legacyCanonical)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(legacyHash), docCompress(legacyCanonical)); err != nil {
		t.Fatalf("legacy blob fixture: %v", err)
	}
	for _, owner := range []domain.ContentHash{repoID, repo2} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'doc',$2)`, string(owner), string(legacyHash)); err != nil {
			t.Fatalf("legacy owner fixture: %v", err)
		}
	}
	manifest, err := st.GetDocManifest(ctx, repoID, legacyHash)
	if err != nil || manifest.Format != domain.ChunkFormatV2 || len(manifest.Chunks) < 2 {
		t.Fatalf("legacy manifest=%+v err=%v", manifest, err)
	}
	for _, owner := range []domain.ContentHash{repoID, repo2} {
		got, err := st.GetDoc(ctx, owner, legacyHash)
		if err != nil || domain.ValidateSessionDocHash(got) != nil {
			t.Fatalf("owner %s lost repacked doc: %v", owner, err)
		}
	}

	// PutDoc and lazy GetDocManifest both acquire doc → chunk locks. Exercise the
	// same legacy row concurrently so an inverted order would surface as a timeout
	// or PostgreSQL deadlock error instead of an intermittent production failure.
	raceCIR := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("lock-order", 96<<10)}}}},
	}
	raceCanonical, _ := domain.CanonicalBytes(raceCIR)
	raceHash := domain.HashContent(raceCanonical)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(raceHash), docCompress(raceCanonical)); err != nil {
		t.Fatalf("lock-order blob fixture: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs (repo_id,kind,hash) VALUES ($1,'doc',$2)`, string(repoID), string(raceHash)); err != nil {
		t.Fatalf("lock-order owner fixture: %v", err)
	}
	raceCtx, cancelRace := context.WithTimeout(ctx, 10*time.Second)
	defer cancelRace()
	startRace := make(chan struct{})
	raceErrs := make(chan error, 2)
	go func() {
		<-startRace
		_, err := st.GetDocManifest(raceCtx, repoID, raceHash)
		raceErrs <- err
	}()
	go func() {
		<-startRace
		_, err := st.PutDoc(raceCtx, repo2, domain.SessionDoc{Hash: raceHash, CIR: raceCIR})
		raceErrs <- err
	}()
	close(startRace)
	for i := 0; i < 2; i++ {
		if err := <-raceErrs; err != nil {
			t.Fatalf("concurrent doc/chunk lock ordering: %v", err)
		}
	}

	// A corrupt globally deduplicated chunk must abort the whole new-doc
	// transaction; the manifest row must not survive the rollback.
	corruptCIR := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("collision", 80<<10)}}}},
	}
	corruptCanonical, _ := domain.CanonicalBytes(corruptCIR)
	corruptHash := domain.HashContent(corruptCanonical)
	corruptPlan, ok := domain.PlanDocChunks(corruptCanonical)
	if !ok {
		t.Fatal("corrupt chunk fixture plan unavailable")
	}
	badChunk := corruptPlan.Order[0]
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs (hash,bytes) VALUES ($1,$2)`, string(badChunk), docCompress([]byte("corrupt body"))); err != nil {
		t.Fatalf("corrupt chunk fixture: %v", err)
	}
	if _, err := st.PutDoc(ctx, repoID, domain.SessionDoc{Hash: corruptHash, CIR: corruptCIR}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupt chunk PutDoc err=%v, want ErrIntegrity", err)
	}
	if _, err := st.GetDoc(ctx, repoID, corruptHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed PutDoc left a visible doc: %v", err)
	}
}
