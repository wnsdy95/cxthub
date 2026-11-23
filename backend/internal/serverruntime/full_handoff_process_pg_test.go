//go:build postgres

package serverruntime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (fixture *executionProcessFixture) fullHandoffReceipt(test *testing.T, ctx context.Context) domain.DocFinalizationJob {
	test.Helper()
	receipt, err := fixture.storage.GetDocJob(executionProcessSystemContext(ctx), fixture.repo, fixture.job.ID)
	if err != nil {
		test.Fatal("read the same original durable handoff receipt", err)
	}
	return receipt
}

func (fixture *executionProcessFixture) assertFullHandoffReceipt(test *testing.T, ctx context.Context, want domain.DocFinalizationJob) {
	test.Helper()
	if current := fixture.fullHandoffReceipt(test, ctx); !reflect.DeepEqual(current, want) {
		test.Fatal("profile handoff or worker replacement rewrote the retained durable receipt", current.State, current.Attempts, current.Version)
	}
}

func (fixture *executionProcessFixture) enqueueFullHandoffDocument(test *testing.T, ctx context.Context) *executionProcessFixture {
	test.Helper()
	ctx = executionProcessSystemContext(ctx)
	recovery := *fixture
	recovery.marker = "full worker useful publication after hard kill \ud55c🙂"
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(fixture.repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: recovery.marker}}}},
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal("construct useful recovery document", err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal("hash useful recovery document", err)
	}
	root, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal("encode useful recovery document", err)
	}
	if _, _, err := fixture.storage.PutChunks(ctx, fixture.repo, bodies); err != nil {
		test.Fatal("upload only original recovery document chunks", err)
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(fixture.repo,
		domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root}, time.Now().UTC())
	if err != nil {
		test.Fatal("construct ordinary durable recovery job", err)
	}
	if _, err := fixture.storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal("enqueue useful recovery work through the unchanged production port", err)
	}
	recovery.job = job
	return &recovery
}

func (fixture *executionProcessFixture) assertFullHandoffUnavailable(test *testing.T, ctx context.Context, api, mcp *executionProcessChild) {
	test.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 51, "method": "tools/call", "params": map[string]any{
		"name": "context_fetch", "arguments": map[string]any{"repository": string(fixture.repo), "ref": string(fixture.job.DocHash), "branch": "main", "limit": 1},
	}})
	if err != nil {
		test.Fatal("encode the actual authorized MCP handoff request")
	}
	for _, child := range []*executionProcessChild{api, mcp} {
		method, path, token, body := http.MethodGet, child.url+"/api/v1/repos/"+string(fixture.repo)+"/docs/"+string(fixture.job.DocHash)+"/events?offset=0&limit=1", fixture.session, []byte(nil)
		if child.role == outbound.ProcessMCP {
			method, path, token, body = http.MethodPost, child.url+"/mcp", fixture.mcpToken, encoded
		}
		request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
		if err != nil {
			test.Fatal("construct the actual handoff protocol request", child.role)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Cxt-Doc-Identities", string(domain.DocumentIdentityRootV1))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		status, response := executionProcessHTTP(test, client, request)
		if status != http.StatusServiceUnavailable || bytes.Contains(response, []byte(fixture.marker)) {
			test.Fatal("incompatible or incomplete roster did not fail closed at the actual protocol boundary", child.role, status)
		}
	}
}
