//go:build postgres

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	httpdelivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type effectiveMemorySourceTransportFixture struct {
	*prContributionParityFixture
	actor     context.Context
	selection domain.EffectiveMemorySelection
	finish    domain.GitScanFinish
}

type effectiveMemorySourceTransportPage struct {
	Mode         string                          `json:"mode"`
	Selection    domain.EffectiveMemorySelection `json:"selection"`
	StateHash    domain.ContentHash              `json:"state_hash"`
	LineageHash  domain.ContentHash              `json:"lineage_hash"`
	Total        int                             `json:"total"`
	ItemID       domain.ContentHash              `json:"item_id"`
	Offset       int                             `json:"byte_offset"`
	Fragment     string                          `json:"json_fragment"`
	ItemComplete bool                            `json:"item_complete"`
	Next         string                          `json:"next_cursor"`
}

func newEffectiveMemorySourceTransportPG(t *testing.T, fragment bool) *effectiveMemorySourceTransportFixture {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	system := inbound.WithSystemActor(ctx)
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	suffix := fmt.Sprintf("memory-source-%d", time.Now().UnixNano())
	owner, session, err := identity.Login(ctx, "dev:"+suffix+"@example.test:Memory Source", "effective memory source test")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(system, owner, "MemorySource")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID,
		DefaultBranch: "main", GitRemoteURL: "https://github.com/example/effective-memory-source.git"}
	if _, err := storage.PutRepo(system, repo); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	fixture := &effectiveMemorySourceTransportFixture{
		prContributionParityFixture: &prContributionParityFixture{ctx: system, storage: storage, service: service,
			repo: repo, httpToken: session.Token, code: strings.Repeat("1", 40)},
		actor: inbound.WithRepositoryActor(ctx, owner.ID),
	}
	fixture.current = fixture.snapshot(t, suffix, "main", time.Now().UTC())
	claim := domain.MemoryClaim{Kind: "code", Text: "Later typed code claim", Code: &domain.MemoryCodeScope{Commit: fixture.code, Paths: []string{"file"}}}
	claims := []domain.MemoryClaim{{Kind: "rationale", Text: "Unchanged first item"}, claim}
	if fragment {
		claim.Text = strings.Repeat("한글\n\"", 900)
		claims = []domain.MemoryClaim{claim, {Kind: "rationale", Text: "Retained explanation"}}
	}
	digest := domain.MemoryDigest{SnapshotID: fixture.current.ID, ClaimsVersion: domain.MemoryClaimsVersion,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: fixture.current.ID, Claims: claims}}}
	memory, err := service.PutMemoryDigestCAS(system, repo.ID, digest)
	if err != nil {
		t.Fatal(err)
	}
	observation := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo.ID), Branch: "main", BranchID: "main-id",
		Kind: "position", Target: fixture.current.ID, GitAfter: fixture.code, CreatedAt: time.Now().UTC()}
	publication := observation
	publication.ID, publication.Kind, publication.Source = strings.Repeat("b", 32), "publish", fixture.current.ID
	publication.MemoryPinned, publication.CreatedAt = true, observation.CreatedAt.Add(time.Second)
	for _, event := range []domain.HistoryEvent{observation, publication} {
		if err := storage.ApplyHistoryEvent(system, event); err != nil {
			t.Fatal(err)
		}
	}
	job := domain.NewGitScan(repo.ID, repo.GitRemoteURL, fixture.code, time.Now().UTC())
	if err := storage.EnqueueGitScan(system, job); err != nil {
		t.Fatal(err)
	}
	job, err = storage.ClaimGitScan(system, repo.ID, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, err := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	if err != nil {
		t.Fatal(err)
	}
	fixture.finish = domain.GitScanFinish{Job: job, Tree: &domain.GitTreeEvidence{
		Commit: domain.GitCommitTree{Commit: fixture.code, Tree: node.OID, Parents: []string{}}, Nodes: []domain.GitTreeNode{node}}}
	fixture.finish.Job.State, fixture.finish.Job.TreeIndexed = "waiting", true
	fixture.selection = domain.EffectiveMemorySelection{SnapshotID: fixture.current.ID, MemoryHash: memory, CodeCommit: fixture.code}
	api := httpdelivery.NewServer(service, identity)
	snapshots := memorySnapshotQueriesForTest(t, service)
	if err := api.SetMemorySnapshotQueries(snapshots); err != nil {
		t.Fatal(err)
	}
	api.SetEffectiveMemory(service)
	fixture.httpHandler = api.Handler()
	server, err := NewServer(service, identity, storage, "https://memory-source.example.test")
	if err != nil {
		t.Fatal(err)
	}
	setLiveMemoryFixture(t, server, service, service)
	if err := server.SetMemorySnapshotQueries(snapshots); err != nil {
		t.Fatal(err)
	}
	fixture.mcpHandler = server.Handler()
	pair, err := identity.IssueMCPTokenPair(system, owner.ID, "memory-source-client", "https://memory-source.example.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	fixture.mcpToken = pair.AccessToken
	return fixture
}

func (fixture *effectiveMemorySourceTransportFixture) readMemoryTool(t *testing.T, args toolArgs) effectiveMemorySourceTransportPage {
	t.Helper()
	response, envelope := fixture.toolRequest(t, fixture.mcpToken, "memory_load", args)
	if response.Code != http.StatusOK || envelope.Error != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "text" {
		t.Fatalf("MCP effective memory: %d %s", response.Code, response.Body.String())
	}
	raw := envelope.Result.Content[0].Text
	if len(raw) > pageBytes || !utf8.ValidString(raw) {
		t.Fatalf("MCP fragment exceeded byte or UTF-8 bounds: %d bytes", len(raw))
	}
	var page effectiveMemorySourceTransportPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestPGEffectiveMemorySourceInvalidatesMCPAndHTTPCursors(t *testing.T) {
	for _, mode := range []string{"fragment", "next_item"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newEffectiveMemorySourceTransportPG(t, mode == "fragment")
			repo := fixture.repo.ID
			args := toolArgs{Mode: "effective", Ref: string(fixture.selection.SnapshotID), MemoryHash: string(fixture.selection.MemoryHash), CodeCommit: fixture.code}
			first := fixture.readMemoryTool(t, args)
			if first.Next == "" || first.ItemComplete != (mode == "next_item") || first.Offset != 0 || first.Fragment == "" {
				t.Fatalf("fixture did not produce %s continuation: %+v", mode, first)
			}
			request := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 1}
			before, err := fixture.service.QueryEffectiveMemory(fixture.actor, repo, request)
			if err != nil || before.NextCursor == "" || before.StateHash != first.StateHash || before.LineageHash != first.LineageHash || before.DeliveryStateHash == "" {
				t.Fatalf("initial direct/MCP generation mismatch: %+v %v", before, err)
			}
			query := url.Values{"snapshot_id": {string(fixture.selection.SnapshotID)}, "memory_hash": {string(fixture.selection.MemoryHash)}, "code_commit": {fixture.code}, "limit": {"1"}}
			var httpBefore domain.EffectiveMemoryPage
			fixture.readHTTP(t, "/effective-memory?"+query.Encode(), &httpBefore)
			if !reflect.DeepEqual(httpBefore, before) {
				t.Fatal("initial HTTP/direct application pages differ")
			}
			sourceBefore, err := fixture.storage.ReadRepositoryGraphSourceGeneration(fixture.ctx, repo)
			if err != nil || sourceBefore.Validate() != nil || sourceBefore.Sequence <= 0 {
				t.Fatalf("missing graph source generation: %+v %v", sourceBefore, err)
			}
			if err := fixture.storage.FinishGitScan(fixture.ctx, fixture.finish); err != nil {
				t.Fatal(err)
			}
			sourceAfter, err := fixture.storage.ReadRepositoryGraphSourceGeneration(fixture.ctx, repo)
			if err != nil || sourceAfter.Epoch != sourceBefore.Epoch || sourceAfter.Sequence <= sourceBefore.Sequence {
				t.Fatalf("evidence-only commit did not advance 0101 source clock: %+v %+v %v", sourceBefore, sourceAfter, err)
			}
			revision, err := fixture.storage.RepositoryRevision(fixture.ctx, repo)
			if err != nil || revision != before.Revision {
				t.Fatalf("fixture advanced legacy G/P/E revisions: %+v %+v %v", before.Revision, revision, err)
			}
			args.Cursor = first.Next
			response, envelope := fixture.toolRequest(t, fixture.mcpToken, "memory_load", args)
			if response.Code != http.StatusOK || envelope.Error != nil || !envelope.Result.IsError || len(envelope.Result.Content) != 1 ||
				!strings.Contains(envelope.Result.Content[0].Text, "effective memory changed") || !strings.Contains(envelope.Result.Content[0].Text, "restart") {
				t.Fatalf("old MCP %s cursor crossed source generations: %d %s", mode, response.Code, response.Body.String())
			}
			request.Cursor = before.NextCursor
			if _, err := fixture.service.QueryEffectiveMemory(fixture.actor, repo, request); !errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
				t.Fatalf("old direct cursor survived evidence-only commit: %v", err)
			}
			query.Set("cursor", before.NextCursor)
			staleHTTP := fixture.httpRequest(t, "/effective-memory?"+query.Encode(), fixture.httpToken)
			if staleHTTP.Code != http.StatusConflict || !strings.Contains(staleHTTP.Body.String(), "memory_cursor_stale") {
				t.Fatalf("old HTTP cursor crossed source generations: %d %s", staleHTTP.Code, staleHTTP.Body.String())
			}
			request.Cursor = ""
			after, err := fixture.service.QueryEffectiveMemory(fixture.actor, repo, request)
			if err != nil || after.StateHash == before.StateHash || after.DeliveryStateHash == before.DeliveryStateHash || after.LineageHash != before.LineageHash || after.Revision != before.Revision {
				t.Fatalf("fresh application proof retained obsolete evidence: %+v %v", after, err)
			}
			if mode == "next_item" && !reflect.DeepEqual(after.Items, before.Items) {
				t.Fatal("next-item fixture changed first-page content rather than only off-page evidence")
			}
			cold := app.NewService(fixture.storage, fixture.storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(fixture.storage), fixture.storage)
			freshRequest := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 50}
			expected, err := cold.QueryEffectiveMemory(fixture.actor, repo, freshRequest)
			if err != nil || expected.StateHash != after.StateHash || expected.DeliveryStateHash != after.DeliveryStateHash || expected.Total != 2 {
				t.Fatalf("warm cache disagrees with cold direct evaluation: %+v %v", expected, err)
			}
			query.Del("cursor")
			query.Set("limit", "50")
			var httpAfter domain.EffectiveMemoryPage
			fixture.readHTTP(t, "/effective-memory?"+query.Encode(), &httpAfter)
			if !reflect.DeepEqual(httpAfter, expected) {
				t.Fatal("fresh HTTP result differs from cold direct application")
			}
			args.Cursor = ""
			var joined strings.Builder
			items := []domain.EffectiveMemoryItem{}
			for pageNumber := 0; pageNumber < 20; pageNumber++ {
				page := fixture.readMemoryTool(t, args)
				if page.StateHash != expected.StateHash || page.LineageHash != expected.LineageHash || page.Selection != expected.Selection || page.Total != expected.Total ||
					page.Mode != "effective" || page.Offset != joined.Len() || !utf8.ValidString(page.Fragment) {
					t.Fatalf("fresh MCP continuation mixed generation, selection, or fragment boundaries: %+v", page)
				}
				joined.WriteString(page.Fragment)
				if page.ItemComplete {
					var item domain.EffectiveMemoryItem
					if err := json.Unmarshal([]byte(joined.String()), &item); err != nil || item.ID != page.ItemID {
						t.Fatalf("invalid reconstructed effective-memory item: %v", err)
					}
					items = append(items, item)
					joined.Reset()
				}
				args.Cursor = page.Next
				if args.Cursor == "" {
					break
				}
			}
			if args.Cursor != "" || joined.Len() != 0 || !reflect.DeepEqual(items, expected.Items) {
				t.Fatal("fresh MCP fragments changed or truncated direct effective-memory assessment")
			}
			t.Logf("%s: graph source %s:%d -> %d; unchanged G/P/E=%+v; stale MCP+HTTP rejected; %d fresh items match cold direct application", mode,
				sourceBefore.Epoch, sourceBefore.Sequence, sourceAfter.Sequence, revision, len(items))
		})
	}
}
