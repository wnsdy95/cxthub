//go:build postgres

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Real PG worker -> tagged snapshot -> service and MCP consumers. No fake index,
// root body, candidate set, authorization source or provider is installed.
func TestRootProjectionPGConsumersWithoutPersistentIndex(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn = rootProjectionIsolatedDSN(t, ctx)
	ids := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	sys := outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), ids), ids, ids)
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	identity := app.NewIdentityService(nil, st)
	suffix := fmt.Sprintf("p%d", time.Now().UnixNano())
	owner := domain.User{ID: "dev:" + suffix, Username: suffix, Name: "Synthetic Owner", Email: suffix + "@example.test"}
	if err = st.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	record, err := identity.CreateRepository(sys, owner, "Projection")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(record.ID)), RepositoryID: record.ID, DefaultBranch: "main"}
	if _, err = st.PutRepo(sys, repo); err != nil {
		t.Fatal(err)
	}
	if err = st.RequireDocumentIdentity(sys, repo.ID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(st, st, nil, nil, st)
	if err = svc.ConfigureConversationRootPublication(true); err != nil {
		t.Fatal(err)
	}
	user := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(ctx, owner.ID), ids)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionOriginID: suffix}, Events: []domain.CIREvent{}}
	// >128 events crosses a shared index block; giant text spans root chunks.
	for i := 0; i < 130; i++ {
		cir.Events = append(cir.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("needle \ud55c 50%%_\\ row %d", i)}, {Type: "text", Text: "joined"}}})
	}
	cir.Events = append(cir.Events, domain.CIREvent{Seq: 130, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("synthetic giant text ", 60000) + "giantneedle"}}})
	cir.Events = append(cir.Events, domain.CIREvent{Seq: 131, Kind: domain.EventReasoning, RedactedSummary: "needle summary"})
	publish := func(cir domain.CIRDocument, parent domain.ContentHash) (domain.Snapshot, domain.ConversationManifest, map[domain.ContentHash][]byte) {
		t.Helper()
		manifest, bodies, e := domain.ConversationManifestForCIR(cir)
		if e != nil {
			t.Fatal(e)
		}
		hash, e := domain.ConversationManifestHash(manifest)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := domain.CanonicalConversationManifest(manifest)
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = st.PutChunks(sys, repo.ID, bodies); e != nil {
			t.Fatal(e)
		}
		job, e := domain.NewDocFinalizationJobForRepresentation(repo.ID, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if _, e = st.EnqueueDocJob(sys, job); e != nil {
			t.Fatal(e)
		}
		if e = svc.ProcessDocFinalizations(sys, 1); e != nil {
			t.Fatal(e)
		}
		done, e := st.GetDocJob(sys, repo.ID, job.ID)
		if e != nil || done.State != "completed" {
			t.Fatal("worker receipt", e)
		}
		snap := domain.Snapshot{ID: hash, RepoID: repo.ID, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}
		if parent != "" {
			snap.Parents = []domain.ContentHash{parent}
		}
		if e = st.PutSnapshot(sys, snap); e != nil {
			t.Fatal(e)
		}
		return snap, manifest, bodies
	}
	base, _, _ := publish(cir, "")
	cir.Events = append(cir.Events, domain.CIREvent{Seq: 132, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle appended"}}})
	snap, manifest, bodies := publish(cir, base.ID)
	assertNoIndex := func() {
		t.Helper()
		var n int
		if e := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM doc_read_indexes_v3)+(SELECT count(*) FROM doc_read_block_locations_v3)+(SELECT count(*) FROM doc_search_events_v2)+(SELECT count(*) FROM doc_read_block_preparations_v3)`).Scan(&n); e != nil || n != 0 {
			t.Fatalf("consumer/publication wrote legacy projection: %d %v", n, e)
		}
	}
	assertNoIndex()
	got, err := svc.GetDoc(user, repo.ID, snap.ID)
	if err != nil || got.DocumentRef() != snap.DocumentRef() {
		t.Fatal("whole root reference", err)
	}
	expectedCanonical, e := domain.CanonicalBytes(cir)
	if e != nil {
		t.Fatal(e)
	}
	actualCanonical, e := domain.CanonicalBytes(got.CIR)
	if e != nil || !bytes.Equal(actualCanonical, expectedCanonical) {
		t.Fatal("whole root canonical bytes", e)
	}
	page, err := svc.ReadDocEvents(user, repo.ID, snap.ID, base.ID, -1, 10)
	if err != nil || page.Inherited != 132 || len(page.Events) != 1 || page.Events[0].Seq != 132 {
		t.Fatal("inherited event page", page.Inherited, err)
	}
	fragment, err := svc.ReadDocFragments(user, repo.ID, snap.ID, 130, 0, 1, 1024)
	if err != nil || len(fragment.Fragments) != 1 || fragment.Fragments[0].Complete {
		t.Fatal("giant fragment", err)
	}
	_, err = svc.ReadAgentHistoryPage(user, repo.ID, snap.ID, domain.AgentHistoryPageRequest{DocIdentity: domain.DocumentIdentityRootV1, CoveredByIdentity: domain.DocumentIdentityRootV1, CoveredBy: base.ID, Before: -1, Limit: 2, MaxBytes: 64 << 10})
	if !errors.Is(err, domain.ErrContextBudgetExceeded) {
		t.Fatal("giant complete turn budget fence", err)
	}
	agent, err := svc.ReadAgentHistoryPage(user, repo.ID, snap.ID, domain.AgentHistoryPageRequest{DocIdentity: domain.DocumentIdentityRootV1, Before: 128, Limit: 2, MaxBytes: 64 << 10})
	if err != nil || agent.Hash != snap.ID {
		t.Fatal("bounded agent history", err)
	}

	for _, query := range []string{"needle", "needle \ud55c", "50%_\\", "row 128\njoined", "giantneedle", "absent"} {
		want := []int{}
		q := strings.ToLower(strings.TrimSpace(query))
		for i, e := range cir.Events {
			if strings.Contains(strings.ToLower(domain.SearchableEventText(e)), q) {
				want = append(want, i)
			}
		}
		var indexes []int
		after := -1
		for {
			hits, e := svc.SearchDocEvents(user, repo.ID, snap.ID, query, after, 37)
			if e != nil {
				t.Fatal(e)
			}
			for _, h := range hits {
				indexes = append(indexes, h.Index)
				after = h.Index
			}
			if len(hits) < 37 {
				break
			}
		}
		if len(indexes) != len(want) || len(want) > 0 && !reflect.DeepEqual(indexes, want) {
			t.Fatalf("exact paged search %q: got %v want %v", query, indexes, want)
		}
	}
	search, err := svc.Search(user, inbound.SearchInput{RepoID: repo.ID, Query: "needle", Limit: 200})
	if err != nil || search.Truncated || len(search.Hits) != 133 {
		t.Fatal("repository search deduplication", len(search.Hits), err)
	}
	for i, hit := range search.Hits {
		owner := base.ID
		if i == 132 {
			owner = snap.ID
		}
		if hit.SnapshotID != owner || hit.Kind != "event" || hit.Seq != i {
			t.Fatal("inherited event attribution", i, hit)
		}
	}
	// Real history publication verifies root closure and records immutable receipts.
	event := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), Branch: "main", BranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), Kind: "position", Source: snap.ID, Target: snap.ID, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
	if err = svc.RecordHistory(user, event); err != nil {
		t.Fatal("history publication", err)
	}
	history, err := svc.ListHistory(user, repo.ID)
	if err != nil || len(history) != 1 || history[0].ID != event.ID {
		t.Fatal("history read", err)
	}
	server, err := NewServer(svc, identity, st, "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	args := func(query, cursor string) json.RawMessage {
		b, e := json.Marshal(toolArgs{Repository: string(repo.ID), Ref: string(snap.ID), Scope: "all", Query: query, Cursor: cursor, Limit: 40})
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	cursor := ""
	count := 0
	seen := map[string]bool{}
	for calls := 0; calls < 10; calls++ {
		raw, e := server.runTool(user, owner, "context_search", args("needle", cursor))
		if e != nil {
			t.Fatal("MCP search", e)
		}
		var out struct {
			Hits []struct {
				Seq      int                `json:"seq"`
				Snapshot domain.ContentHash `json:"snapshot_id"`
				Kind     string             `json:"kind"`
			} `json:"hits"`
			Next string `json:"next_cursor"`
		}
		if e = json.Unmarshal([]byte(raw), &out); e != nil {
			t.Fatal(e)
		}
		for _, hit := range out.Hits {
			key := fmt.Sprintf("%s/%d", hit.Snapshot, hit.Seq)
			max := 132
			if hit.Snapshot == snap.ID {
				max = 133
			} else if hit.Snapshot != base.ID {
				t.Fatal("foreign MCP result", hit)
			}
			if hit.Kind != "event" || hit.Seq < 0 || hit.Seq >= max || seen[key] {
				t.Fatal("MCP ordinal/pagination", hit)
			}
			seen[key] = true
			count++
		}
		cursor = out.Next
		if cursor == "" {
			break
		}
	}
	if cursor != "" || count != 265 {
		t.Fatalf("MCP completeness: %d next=%q", count, cursor)
	}
	raw, err := server.runTool(user, owner, "context_history", args("", ""))
	if err != nil || !strings.Contains(raw, event.ID) {
		t.Fatal("MCP history", err)
	}
	raw, err = server.runTool(user, owner, "context_fetch", args("", ""))
	if err != nil || !strings.Contains(raw, string(snap.ID)) {
		t.Fatal("MCP fetch", err)
	}
	foreign := domain.User{ID: "dev:outsider", Username: "outsider"}
	if _, err = server.runTool(inbound.WithRepositoryActor(ctx, foreign.ID), foreign, "context_search", args("needle", "")); err == nil {
		t.Fatal("MCP admitted outsider")
	}
	if _, err = svc.PutMemoryDigestCAS(user, repo.ID, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "synthetic root memory"}); err != nil {
		t.Fatal("root memory publication", err)
	}
	raw, err = server.runTool(user, owner, "memory_load", args("", ""))
	if err != nil || !strings.Contains(raw, "synthetic root memory") {
		t.Fatal("MCP root memory", err)
	}
	if err = st.BackfillReadIndexes(sys, func(int) {}); err != nil {
		t.Fatal("root backfill", err)
	}
	assertNoIndex()
	// Reopen without any caches/indexes; a corrupt unrequested tail must block
	// no-hit search and new history publication, leaving its receipt absent.
	reopened, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.ReadVerifiedDoc(ctx, repo.ID, snap.ID); err != nil {
		t.Fatal("restart", err)
	}
	tail := manifest.Chunks[len(manifest.Chunks)-1].Hash
	if _, err = db.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, []byte("corrupt synthetic tail")); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SearchDocEvents(user, repo.ID, snap.ID, "absent", -1, 10); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("no-hit search skipped corrupted tail")
	}
	event.ID = strings.Repeat("2", 32)
	event.CreatedAt = time.Now().UTC()
	if err = svc.RecordHistory(user, event); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("history lost corrupt current root error", err)
	}
	history, err = svc.ListHistory(user, repo.ID)
	if err != nil || len(history) != 1 {
		t.Fatal("failed history changed receipts", err)
	}
	if _, err = db.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, bodies[tail]); err != nil {
		t.Fatal(err)
	}
	// User authorization belongs to the real delivery boundary; storage and
	// service reads additionally preserve the requested repository ownership.
	if _, err = server.runTool(inbound.WithRepositoryActor(ctx, foreign.ID), foreign, "context_fetch", args("", "")); err == nil {
		t.Fatal("MCP outsider fetched restored root")
	}
	if _, err = svc.SearchDocEvents(user, domain.HashContent([]byte("unowned repository")), snap.ID, "needle", -1, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign repository ownership", err)
	}

	assertNoIndex()
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
