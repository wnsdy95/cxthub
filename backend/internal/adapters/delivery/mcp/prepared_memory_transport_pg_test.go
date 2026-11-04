//go:build postgres

package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	httpdelivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedMemoryTransportPage struct {
	effectiveMemorySourceTransportPage
	Status      domain.MemoryReadStatus `json:"status"`
	ReadVersion int                     `json:"read_version"`
}

func TestPGPreparedMemoryTransportRejectedComparisonIsTerminal(test *testing.T) {
	fixture := newEffectiveMemorySourceTransportPG(test, false)
	repo := fixture.repo.ID
	if err := fixture.service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
	api := httpdelivery.NewServer(fixture.service, identity)
	api.SetEffectiveMemory(fixture.service)
	reader := fixture.preparedReader(test, fixture.service)
	if err := api.SetPreparedMemory(reader); err != nil {
		test.Fatal(err)
	}
	fixture.httpHandler = api.Handler()
	server, err := NewServer(fixture.service, identity, fixture.storage, "https://memory-source.example.test")
	if err != nil {
		test.Fatal(err)
	}
	setLiveMemoryFixture(test, server, fixture.service, fixture.service)
	if err := server.SetPreparedMemory(reader); err != nil {
		test.Fatal(err)
	}
	fixture.mcpHandler = server.Handler()
	if err := fixture.service.ConfigureMemoryReadPreparations(fixture.ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := fixture.storage.ConfigureMemoryReadWork(fixture.ctx, repo, false); err != nil {
			test.Error(err)
		}
	})
	request := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 1}
	if _, err := fixture.service.RequestMemoryReadPreparation(fixture.ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessMemoryReadPreparations(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	query := url.Values{"snapshot_id": {string(fixture.selection.SnapshotID)}, "memory_hash": {string(fixture.selection.MemoryHash)}, "code_commit": {fixture.code}, "limit": {"1"}}
	path := func() string { return "/effective-memory/read?" + query.Encode() }
	var first domain.MemoryReadPage
	fixture.readHTTP(test, path(), &first)
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.selection.SnapshotID), MemoryHash: string(fixture.selection.MemoryHash), CodeCommit: fixture.code}
	firstMCP := fixture.readPreparedTool(test, args)
	if err := fixture.storage.FinishGitScan(fixture.ctx, fixture.finish); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.service.RequestMemoryReadPreparation(fixture.ctx, repo, request); err != nil {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimMemoryReadWork(fixture.ctx, []domain.ContentHash{repo}, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.FailMemoryReadWork(fixture.ctx, job, "comparison_mismatch", false); err != nil {
		test.Fatal(err)
	}
	response := fixture.httpRequest(test, path(), fixture.httpToken)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "memory_read_needs_attention") ||
		response.Header().Get("Retry-After") != "" || strings.Contains(response.Body.String(), "retry_cursor") || strings.Contains(response.Body.String(), "items") {
		test.Fatal("HTTP served a rejected comparison or requested retries", response.Code, response.Body.String())
	}
	if failure := fixture.preparedToolFailure(test, args, "memory_read_needs_attention"); failure.Retryable || failure.RetryCursor != "" {
		test.Fatal("MCP requested retries after a rejected comparison")
	}
	query.Set("cursor", first.Cursor)
	var replay domain.MemoryReadPage
	fixture.readHTTP(test, path(), &replay)
	if !reflect.DeepEqual(replay.Page, first.Page) || replay.Status.Generation != first.Status.Generation {
		test.Fatal("new comparison failure invalidated earlier accepted output")
	}
	args.Cursor = firstMCP.Next
	if next := fixture.readPreparedTool(test, args); next.Status.Generation != firstMCP.Status.Generation {
		test.Fatal("rejection moved pinned MCP continuation")
	}
}

func (fixture *effectiveMemorySourceTransportFixture) readPreparedTool(test *testing.T, args toolArgs) preparedMemoryTransportPage {
	test.Helper()
	response, envelope := fixture.toolRequest(test, fixture.mcpToken, "memory_load", args)
	if response.Code != http.StatusOK || envelope.Error != nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		test.Fatalf("prepared MCP read: %d %s", response.Code, response.Body.String())
	}
	raw := envelope.Result.Content[0].Text
	if len(raw) > pageBytes || !utf8.ValidString(raw) {
		test.Fatal("prepared fragment exceeded byte or UTF-8 bound", len(raw))
	}
	var page preparedMemoryTransportPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		test.Fatal(err)
	}
	if page.ReadVersion != 1 || page.Status.Validate() != nil {
		test.Fatal("prepared result omitted its version or source proof")
	}
	return page
}

func (fixture *effectiveMemorySourceTransportFixture) preparedToolFailure(test *testing.T, args toolArgs, code string) preparedMemoryToolError {
	test.Helper()
	response, envelope := fixture.toolRequest(test, fixture.mcpToken, "memory_load", args)
	if response.Code != http.StatusOK || envelope.Error != nil || !envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		test.Fatalf("prepared MCP failure: %d %s", response.Code, response.Body.String())
	}
	var failure struct {
		Error preparedMemoryToolError `json:"error"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &failure); err != nil || failure.Error.Code != code {
		test.Fatalf("prepared MCP error code: %s %v", response.Body.String(), err)
	}
	return failure.Error
}

func TestPGPreparedMemoryTransportPinnedParityAndRevocation(test *testing.T) {
	for _, mode := range []string{"fragment", "next_item"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newEffectiveMemorySourceTransportPG(test, mode == "fragment")
			identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
			api := httpdelivery.NewServer(fixture.service, identity)
			api.SetEffectiveMemory(fixture.service)
			reader := fixture.preparedReader(test, fixture.service)
			if err := api.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			fixture.httpHandler = api.Handler()
			server, err := NewServer(fixture.service, identity, fixture.storage, "https://memory-source.example.test")
			if err != nil {
				test.Fatal(err)
			}
			server.SetEffectiveMemory(fixture.service)
			if err := server.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			fixture.mcpHandler = server.Handler()
			repo := fixture.repo.ID
			version := 1
			args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.selection.SnapshotID), MemoryHash: string(fixture.selection.MemoryHash), CodeCommit: fixture.code}
			query := url.Values{"snapshot_id": {string(fixture.selection.SnapshotID)}, "memory_hash": {string(fixture.selection.MemoryHash)}, "code_commit": {fixture.code}, "limit": {"1"}}
			path := func() string { return "/effective-memory/read?" + query.Encode() }
			if response := fixture.httpRequest(test, path(), fixture.httpToken); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "prepared_memory_unavailable") {
				test.Fatal("HTTP prepared read enabled by default", response.Code, response.Body.String())
			}
			fixture.preparedToolFailure(test, args, "prepared_memory_unavailable")
			if err := fixture.service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			reader = fixture.preparedReader(test, fixture.service)
			if err := api.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			if err := server.SetPreparedMemory(reader); err != nil {
				test.Fatal(err)
			}
			if response := fixture.httpRequest(test, path(), fixture.httpToken); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "prepared_memory_unavailable") || response.Header().Get("Retry-After") != "" {
				test.Fatal("missing worker scope advertised retryable preparation", response.Code, response.Body.String())
			}
			if failure := fixture.preparedToolFailure(test, args, "prepared_memory_unavailable"); failure.Retryable || failure.RetryCursor != "" {
				test.Fatal("missing worker scope issued a pending retry")
			}
			if err := fixture.service.ConfigureMemoryReadPreparations(fixture.ctx, []domain.ContentHash{repo}); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				if err := fixture.storage.ConfigureMemoryReadWork(fixture.ctx, repo, false); err != nil {
					test.Error(err)
				}
			})
			request := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 50}
			expected, err := fixture.service.QueryEffectiveMemory(fixture.actor, repo, request)
			if err != nil || len(expected.Items) != 2 {
				test.Fatal("legacy fixture", err)
			}
			before := fixture.repositoryState(test)
			pendingHTTP := fixture.httpRequest(test, path(), fixture.httpToken)
			var failure struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						RetryCursor string `json:"retry_cursor"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(pendingHTTP.Body.Bytes(), &failure); err != nil || pendingHTTP.Code != http.StatusServiceUnavailable || failure.Error.Code != "memory_read_not_ready" || failure.Error.Details.RetryCursor == "" || pendingHTTP.Header().Get("Retry-After") == "" {
				test.Fatal("HTTP did not preserve initial minimum", pendingHTTP.Code, pendingHTTP.Body.String(), err)
			}
			pendingMCP := fixture.preparedToolFailure(test, args, "memory_read_not_ready")
			if pendingMCP.RetryCursor == "" || !pendingMCP.Retryable {
				test.Fatal("MCP did not preserve initial minimum")
			}
			if _, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("transport read created preparation work", err)
			}
			if _, err := fixture.service.RequestMemoryReadPreparation(fixture.ctx, repo, request); err != nil {
				test.Fatal(err)
			}
			if err := fixture.service.ProcessMemoryReadPreparations(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			query.Set("cursor", failure.Error.Details.RetryCursor)
			var firstHTTP domain.MemoryReadPage
			fixture.readHTTP(test, path(), &firstHTTP)
			args.Cursor = pendingMCP.RetryCursor
			firstMCP := fixture.readPreparedTool(test, args)
			if firstHTTP.Status != firstMCP.Status || firstMCP.StateHash != expected.StateHash || firstMCP.LineageHash != expected.LineageHash || firstMCP.Selection != expected.Selection || firstMCP.Total != 2 || firstHTTP.Cursor == "" || firstHTTP.Page.NextCursor == "" || firstMCP.Next == "" || firstMCP.ItemComplete != (mode == "next_item") || !reflect.DeepEqual(firstHTTP.Page.Items, expected.Items[:1]) {
				test.Fatal("HTTP/MCP/legacy initial proof or page differs", firstHTTP.Status, firstMCP.Status)
			}
			if !bytes.Equal(before, fixture.repositoryState(test)) {
				test.Fatal("preparation or transport changed business source")
			}
			if err := fixture.storage.FinishGitScan(fixture.ctx, fixture.finish); err != nil {
				test.Fatal(err)
			}
			args.Cursor = ""
			newPending := fixture.preparedToolFailure(test, args, "memory_read_not_ready")
			if err := fixture.service.ProcessMemoryReadPreparations(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			before = fixture.repositoryState(test)
			query.Set("cursor", firstHTTP.Cursor)
			var replay domain.MemoryReadPage
			fixture.readHTTP(test, path(), &replay)
			if replay.Status.Generation != firstHTTP.Status.Generation || replay.Status.Minimum != firstHTTP.Status.Minimum || replay.Status.Freshness != "catching_up" || !reflect.DeepEqual(replay.Page, firstHTTP.Page) {
				test.Fatal("HTTP replay moved to latest generation")
			}
			query.Set("cursor", firstHTTP.Page.NextCursor)
			var secondHTTP domain.MemoryReadPage
			fixture.readHTTP(test, path(), &secondHTTP)
			if secondHTTP.Status.Generation != firstHTTP.Status.Generation || !reflect.DeepEqual(secondHTTP.Page.Items, expected.Items[1:]) || secondHTTP.Page.NextCursor != "" {
				test.Fatal("HTTP continuation changed its original source")
			}
			page := firstMCP
			var joined strings.Builder
			var items []domain.EffectiveMemoryItem
			for pageNumber := 0; ; pageNumber++ {
				if pageNumber > 30 || page.Status.Generation != firstMCP.Status.Generation || page.Status.Minimum != firstMCP.Status.Minimum || page.StateHash != expected.StateHash || page.Offset != joined.Len() || page.Fragment == "" {
					test.Fatal("MCP fragment crossed pinned source or lost ordering", pageNumber, page.Offset, joined.Len())
				}
				joined.WriteString(page.Fragment)
				if page.ItemComplete {
					var item domain.EffectiveMemoryItem
					if err := json.Unmarshal([]byte(joined.String()), &item); err != nil || item.ID != page.ItemID {
						test.Fatal("MCP reconstructed invalid item", err)
					}
					items = append(items, item)
					joined.Reset()
				}
				if page.Next == "" {
					break
				}
				args.Cursor = page.Next
				page = fixture.readPreparedTool(test, args)
			}
			if joined.Len() != 0 || !reflect.DeepEqual(items, expected.Items) {
				test.Fatal("MCP fragmented assessment differs from original legacy output")
			}
			args.Cursor = newPending.RetryCursor
			fresh := fixture.readPreparedTool(test, args)
			if fresh.Status.Generation == firstMCP.Status.Generation || fresh.StateHash == firstMCP.StateHash || fresh.Status.Freshness != "current" {
				test.Fatal("new read retained obsolete source proof")
			}
			current, err := fixture.service.QueryEffectiveMemory(fixture.actor, repo, request)
			if err != nil || fresh.StateHash != current.StateHash || fresh.LineageHash != current.LineageHash {
				test.Fatal("fresh prepared/legacy source mismatch", err)
			}
			if !bytes.Equal(before, fixture.repositoryState(test)) {
				test.Fatal("prepared transport wrote business source")
			}
			if err := fixture.storage.ConfigureMemoryReadWork(fixture.ctx, repo, false); err != nil {
				test.Fatal(err)
			}
			if response := fixture.httpRequest(test, path(), fixture.httpToken); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "prepared_memory_unavailable") || response.Header().Get("Retry-After") != "" {
				test.Fatal("disabled scope presented a stale or retryable cursor", response.Code, response.Body.String())
			}
			for _, cursor := range []string{pendingMCP.RetryCursor, firstMCP.Next} {
				args.Cursor = cursor
				if failure := fixture.preparedToolFailure(test, args, "prepared_memory_unavailable"); failure.Retryable || failure.RetryCursor != "" {
					test.Fatal("disabled scope issued a pending retry")
				}
			}
			if err := identity.RevokeMCPToken(fixture.ctx, fixture.mcpToken, "memory-source-client"); err != nil {
				test.Fatal(err)
			}
			args.Cursor = firstMCP.Next
			if response, _ := fixture.toolRequest(test, fixture.mcpToken, "memory_load", args); response.Code != http.StatusUnauthorized {
				test.Fatal("revoked MCP grant retained cursor access", response.Code, response.Body.String())
			}
			if err := identity.Logout(fixture.ctx, fixture.httpToken); err != nil {
				test.Fatal(err)
			}
			if response := fixture.httpRequest(test, path(), fixture.httpToken); response.Code != http.StatusUnauthorized {
				test.Fatal("revoked HTTP session retained cursor access", response.Code, response.Body.String())
			}
		})
	}
}
