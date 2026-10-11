//go:build postgres

package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type prParityRPCResponse struct {
	Error  *rpcError `json:"error"`
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

type prParityMemoryPage struct {
	Mode         string                          `json:"mode"`
	Snapshot     domain.ContentHash              `json:"snapshot_id"`
	MemoryHash   domain.ContentHash              `json:"memory_hash"`
	StateHash    domain.ContentHash              `json:"state_hash"`
	LineageHash  domain.ContentHash              `json:"lineage_hash"`
	Fragment     string                          `json:"json_fragment"`
	Next         string                          `json:"next_cursor"`
	Complete     bool                            `json:"complete"`
	ItemComplete bool                            `json:"item_complete"`
	Total        int                             `json:"total"`
	Selection    domain.EffectiveMemorySelection `json:"selection"`
	Inclusion    struct {
		BranchID string         `json:"branch_id"`
		Code     string         `json:"code_commit"`
		Merges   map[string]int `json:"merges"`
	} `json:"inclusion"`
}

func TestPRContributionTransportParity(t *testing.T) {
	fixture := newPRContributionParityFixture(t)
	t.Run("historical_contribution", func(t *testing.T) { fixture.assertParity(t, true) })
	t.Run("authorization", func(t *testing.T) {
		before := fixture.repositoryState(t)
		paths := []string{
			"/context-query?scope=current&branch=main&position=main",
			"/effective-memory?" + fixture.memoryQuery().Encode(),
			"/memory-objects/" + url.PathEscape(string(fixture.archiveHash)),
		}
		for _, path := range paths {
			response := fixture.httpRequest(t, path, fixture.outsiderHTTP)
			if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "retained historical claim") || strings.Contains(response.Body.String(), "removed archive claim") {
				t.Fatalf("HTTP outsider read %s: %d %s", path, response.Code, response.Body.String())
			}
		}
		for _, call := range []struct {
			name string
			args toolArgs
		}{
			{"context_list", toolArgs{Scope: "current", Branch: "main", Position: "main"}},
			{"memory_load", toolArgs{Ref: "main"}},
			{"memory_load", toolArgs{Ref: "main", Mode: "effective", CodeCommit: fixture.code}},
			{"memory_load", toolArgs{Ref: string(fixture.contribution.Capture), Mode: "stored", MemoryHash: string(fixture.archiveHash)}},
		} {
			response, envelope := fixture.toolRequest(t, fixture.outsiderMCP, call.name, call.args)
			if response.Code != http.StatusOK || envelope.Error != nil || !envelope.Result.IsError || !strings.Contains(response.Body.String(), "not authorized") || strings.Contains(response.Body.String(), "retained historical claim") || strings.Contains(response.Body.String(), "removed archive claim") {
				t.Fatalf("MCP outsider read %s: %d %s", call.name, response.Code, response.Body.String())
			}
		}
		if response := fixture.httpRequest(t, paths[0], ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP anonymous read: %d %s", response.Code, response.Body.String())
		}
		if response, _ := fixture.toolRequest(t, "", "context_list", toolArgs{Scope: "current", Position: "main"}); response.Code != http.StatusUnauthorized {
			t.Fatalf("MCP anonymous read: %d %s", response.Code, response.Body.String())
		}
		if !bytes.Equal(before, fixture.repositoryState(t)) {
			t.Fatal("unauthorized transport requests changed repository state")
		}
	})
	conflict := fixture.observation
	conflict.ID, conflict.BranchID = strings.Repeat("4", 32), "ambiguous-feature-context"
	if err := fixture.service.RecordHistory(fixture.ctx, conflict); err != nil {
		t.Fatal(err)
	}
	t.Run("late_ambiguity", func(t *testing.T) { fixture.assertParity(t, false) })
}

func (fixture *prContributionParityFixture) assertParity(t *testing.T, included bool) {
	t.Helper()
	before := fixture.repositoryState(t)
	var revision domain.RepositoryRevision
	fixture.readHTTP(t, "/revision", &revision)
	var contextView domain.ContextQueryView
	fixture.readHTTP(t, "/context-query?scope=current&branch=main&position=main", &contextView)
	if contextView.Revision != revision || contextView.Position != fixture.current.ID || contextView.Inclusion == nil || contextView.Inclusion.CodeCommit != fixture.code || len(contextView.Inclusion.Merges) != 1 {
		t.Fatalf("HTTP branch context: %+v", contextView)
	}
	merge := contextView.Inclusion.Merges[0]
	state := "review"
	if included {
		state = "included"
	}
	if merge.EventID != fixture.contribution.ID || merge.Source != fixture.contribution.Capture || merge.MergeSHA != fixture.contribution.PR.MergeSHA || merge.PRNumber != fixture.contribution.PR.Number || merge.Before != "" || merge.State != state {
		t.Fatalf("HTTP semantic merge: %+v", merge)
	}
	if !included && merge.Reason != "participation_evidence_conflict" {
		t.Fatalf("late ambiguity was not revalidated: %+v", merge)
	}
	expectedIDs := []domain.ContentHash{}
	for _, snapshot := range contextView.Snapshots {
		expectedIDs = append(expectedIDs, snapshot.ID)
	}
	if slices.Contains(expectedIDs, fixture.contribution.Capture) != included {
		t.Fatalf("HTTP contribution inclusion: %v", expectedIDs)
	}
	actualIDs := []domain.ContentHash{}
	args := toolArgs{Scope: "current", Branch: "main", Position: "main", Limit: 1}
	for pageNumber := 0; pageNumber <= len(expectedIDs); pageNumber++ {
		var page struct {
			Position  domain.ContentHash `json:"position"`
			StateHash domain.ContentHash `json:"state_hash"`
			Branch    string             `json:"branch"`
			Order     string             `json:"order"`
			Snapshots []domain.Snapshot  `json:"snapshots"`
			Next      string             `json:"next_cursor"`
		}
		fixture.readTool(t, "context_list", args, &page)
		if page.StateHash != contextView.StateHash || page.Position != contextView.Position || page.Branch != "main" || page.Order != "git_integration" || len(page.Snapshots) != 1 {
			t.Fatalf("MCP context generation/order: %+v", page)
		}
		actualIDs = append(actualIDs, page.Snapshots[0].ID)
		args.Cursor = page.Next
		if args.Cursor == "" {
			break
		}
	}
	if args.Cursor != "" || !reflect.DeepEqual(actualIDs, expectedIDs) {
		t.Fatalf("MCP/HTTP branch list mismatch: %v != %v", actualIDs, expectedIDs)
	}
	var effective domain.EffectiveMemoryPage
	fixture.readHTTP(t, "/effective-memory?"+fixture.memoryQuery().Encode(), &effective)
	if effective.Revision != revision || !reflect.DeepEqual(effective.Inclusion, contextView.Inclusion) {
		t.Fatalf("HTTP memory/context generation mismatch: %+v", effective)
	}
	var memory prParityMemoryPage
	fixture.readTool(t, "memory_load", toolArgs{Ref: "main", Mode: "effective", CodeCommit: fixture.code}, &memory)
	if memory.Mode != "effective" || memory.StateHash != effective.StateHash || memory.LineageHash != effective.LineageHash || memory.Selection != effective.Selection || memory.Total != effective.Total || memory.Next != "" || memory.Inclusion.BranchID != contextView.Inclusion.BranchID || memory.Inclusion.Code != fixture.code || memory.Inclusion.Merges[state] != 1 {
		t.Fatalf("MCP/HTTP effective memory mismatch: %+v %+v", memory, effective)
	}
	if included {
		if effective.Total != 1 || len(effective.Items) != 1 || effective.NextCursor != "" || !memory.ItemComplete {
			t.Fatalf("pinned claim was omitted or supplemented by mutable memory: %+v", effective)
		}
		item := effective.Items[0]
		if item.Text != "retained historical claim" || item.State != "applied" || item.SourceSnapshot != fixture.contribution.Capture || item.IntegrationReceipt != fixture.contribution.ID || !slices.Contains(item.PublicationIDs, fixture.publication.ID) || len(item.Paths) != 1 || item.Paths[0].Path != "file" {
			t.Fatalf("historical semantic proof lost at HTTP boundary: %+v", item)
		}
		var transported domain.EffectiveMemoryItem
		if err := json.Unmarshal([]byte(memory.Fragment), &transported); err != nil || !reflect.DeepEqual(transported, item) {
			t.Fatalf("MCP changed effective assessment: %+v %v", transported, err)
		}
	} else if effective.Total != 0 || len(effective.Items) != 0 || memory.Fragment != "" {
		t.Fatalf("invalidated contribution memory survived: %+v %+v", effective, memory)
	}
	var projected prParityMemoryPage
	fixture.readTool(t, "memory_load", toolArgs{Ref: "main"}, &projected)
	if included {
		projection, err := fixture.service.QueryBranchMemory(fixture.ctx, fixture.repo.ID, "main", fixture.current.ID, fixture.code)
		if err != nil {
			t.Fatal(err)
		}
		want := domain.PromptStructuredProjection(domain.MergeDigests(domain.MemoryDigest{}, projection.Digest))
		wantRaw, err := json.Marshal(want)
		if err != nil || !bytes.Equal([]byte(projected.Fragment), wantRaw) {
			t.Fatalf("MCP branch memory changed projection bytes: got %s want %s: %v", projected.Fragment, wantRaw, err)
		}
		if projected.Mode != "project" || !projected.Complete || projected.Next != "" || projected.LineageHash != effective.LineageHash || projected.Inclusion.Merges["included"] != 1 {
			t.Fatalf("MCP branch memory generation mismatch: %+v HTTP lineage %s", projected, effective.LineageHash)
		}
		if strings.Contains(projected.Fragment, "unpublished mutable claim") || strings.Contains(projected.Fragment, "removed archive claim") || !strings.Contains(projected.Fragment, "retained historical claim") {
			t.Fatalf("MCP branch memory selected wrong version: %s", projected.Fragment)
		}
	} else if projected.Mode != "project" || projected.Fragment != "" {
		t.Fatalf("MCP branch memory fell back after invalidation: %+v", projected)
	}
	fixture.assertArchive(t)
	if !bytes.Equal(before, fixture.repositoryState(t)) {
		t.Fatal("transport reads mutated repository state")
	}
}

func (fixture *prContributionParityFixture) assertArchive(t *testing.T) {
	t.Helper()
	var archive domain.MemoryDigest
	fixture.readHTTP(t, "/memory-objects/"+url.PathEscape(string(fixture.archiveHash)), &archive)
	var stored prParityMemoryPage
	fixture.readTool(t, "memory_load", toolArgs{Ref: string(fixture.contribution.Capture), Mode: "stored", MemoryHash: string(fixture.archiveHash)}, &stored)
	var transported domain.MemoryDigest
	if err := json.Unmarshal([]byte(stored.Fragment), &transported); err != nil {
		t.Fatal(err)
	}
	hash, err := domain.MemoryDigestHash(transported)
	if err != nil || hash != fixture.archiveHash || stored.MemoryHash != fixture.archiveHash || stored.Mode != "stored" || stored.Snapshot != fixture.contribution.Capture || stored.Next != "" || !stored.Complete || !reflect.DeepEqual(archive, fixture.archive) || !reflect.DeepEqual(transported, archive) || !strings.Contains(stored.Fragment, "removed archive claim") {
		t.Fatalf("exact stored hash changed at transport boundary: %+v %v", stored, err)
	}
}

func (fixture *prContributionParityFixture) memoryQuery() url.Values {
	return url.Values{"branch": {"main"}, "snapshot_id": {string(fixture.current.ID)}, "code_commit": {fixture.code}, "limit": {"1"}}
}

func (fixture *prContributionParityFixture) httpRequest(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return mcpRequest(t, fixture.httpHandler, http.MethodGet, "/api/v1/repos/"+url.PathEscape(string(fixture.repo.ID))+path, nil, headers)
}

func (fixture *prContributionParityFixture) readHTTP(t *testing.T, path string, value any) {
	t.Helper()
	response := fixture.httpRequest(t, path, fixture.httpToken)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %s: %d %s", path, response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prContributionParityFixture) toolRequest(t *testing.T, token, name string, args toolArgs) (*httptest.ResponseRecorder, prParityRPCResponse) {
	t.Helper()
	args.Repository = string(fixture.repo.ID)
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	response := mcpRequest(t, fixture.mcpHandler, http.MethodPost, "/mcp", bytes.NewReader(payload), remoteMCPHeaders(token))
	var envelope prParityRPCResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return response, envelope
}

func (fixture *prContributionParityFixture) readTool(t *testing.T, name string, args toolArgs, value any) {
	t.Helper()
	response, envelope := fixture.toolRequest(t, fixture.mcpToken, name, args)
	if response.Code != http.StatusOK || envelope.Error != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "text" {
		t.Fatalf("MCP %s: %d %s", name, response.Code, response.Body.String())
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), value); err != nil {
		t.Fatal(err)
	}
}

func (fixture *prContributionParityFixture) repositoryState(t *testing.T) []byte {
	t.Helper()
	history, err := fixture.service.ListHistory(fixture.ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := fixture.storage.ListRefs(fixture.ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(fixture.ctx, fixture.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	contributions, err := fixture.storage.ListPRContributions(fixture.ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := fixture.storage.ListPRContributionMemories(fixture.ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{history, refs, snapshots, contributions, pins, revision})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
