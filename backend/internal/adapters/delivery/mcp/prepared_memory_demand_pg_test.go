//go:build postgres

package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	httpdelivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGPreparedMemoryTransportAutomaticAPIDemandAndReadOnlyMCP(test *testing.T) {
	fixture := newEffectiveMemorySourceTransportPG(test, false)
	repo := fixture.repo.ID
	identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
	reader := app.NewService(fixture.storage, fixture.storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(fixture.storage), fixture.storage)
	for _, service := range []*app.Service{fixture.service, reader} {
		if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{repo}); err != nil {
			test.Fatal(err)
		}
	}
	if err := fixture.service.ConfigureMemoryReadPreparations(fixture.ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ConfigurePreparedMemoryDemands([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	api := httpdelivery.NewServer(fixture.service, identity)
	if err := api.SetPreparedMemory(fixture.preparedReader(test, fixture.service)); err != nil {
		test.Fatal(err)
	}
	fixture.httpHandler = api.Handler()
	server, err := NewServer(reader, identity, fixture.storage, "https://memory-source.example.test")
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetPreparedMemory(fixture.preparedReader(test, reader)); err != nil {
		test.Fatal(err)
	}
	fixture.mcpHandler = server.Handler()
	test.Cleanup(func() {
		if err := fixture.storage.ConfigureMemoryReadWork(fixture.ctx, repo, false); err != nil {
			test.Error(err)
		}
	})
	version := 1
	args := toolArgs{Mode: "effective", ReadVersion: &version, Ref: string(fixture.selection.SnapshotID), MemoryHash: string(fixture.selection.MemoryHash), CodeCommit: fixture.code}
	pendingMCP := fixture.preparedToolFailure(test, args, "memory_read_not_ready")
	request := domain.EffectiveMemoryRequest{Selection: fixture.selection, Limit: 1}
	if _, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("MCP created API-owned work", err)
	}
	query := url.Values{"snapshot_id": {string(fixture.selection.SnapshotID)}, "memory_hash": {string(fixture.selection.MemoryHash)}, "code_commit": {fixture.code}, "limit": {"1"}}
	path := func() string { return "/effective-memory/read?" + query.Encode() }
	response := fixture.httpRequest(test, path(), fixture.httpToken)
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				RetryCursor string `json:"retry_cursor"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != http.StatusServiceUnavailable || failure.Error.Code != "memory_read_not_ready" || failure.Error.Details.RetryCursor == "" {
		test.Fatal("HTTP cold read did not register demand", response.Code, response.Body.String(), err)
	}
	work, err := fixture.service.ReadMemoryPreparationStatus(fixture.ctx, repo, request)
	if err != nil || work.State != "waiting" {
		test.Fatal("API request failed to register target", err)
	}
	if err := fixture.service.ProcessMemoryReadPreparations(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	query.Set("cursor", failure.Error.Details.RetryCursor)
	var page domain.MemoryReadPage
	fixture.readHTTP(test, path(), &page)
	args.Cursor = pendingMCP.RetryCursor
	tool := fixture.readPreparedTool(test, args)
	if page.Status != tool.Status || page.Page.StateHash != tool.StateHash || !page.Status.Complete {
		test.Fatal("automatic preparation differs between API and MCP", page.Status, tool.Status)
	}
}
