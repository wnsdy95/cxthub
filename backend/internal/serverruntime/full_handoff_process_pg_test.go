//go:build postgres

package serverruntime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestFullExecutionProcessesPGMixedProtocolKillAndRollback(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := newExecutionProcessFixture(test, ctx)
	documents := *fixture
	originalManifest, err := os.ReadFile(documents.budgetPath)
	if err != nil {
		test.Fatal("retain the original documents manifest for rollback")
	}
	fixture.activateFullProfile(test)
	fullManifest, err := os.ReadFile(fixture.budgetPath)
	if err != nil {
		test.Fatal("read the activated full manifest")
	}
	budget, err := executionprofile.Parse(fullManifest)
	if err != nil || budget.Profile != outbound.BoundedWorkersProfile {
		test.Fatal("handoff did not activate a valid full profile")
	}
	budgetFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal("full profile did not retain its distinct validated fingerprint")
	}
	fullFingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint)
	if err != nil || fullFingerprint != fixture.fingerprint || fullFingerprint == documents.fingerprint {
		test.Fatal("full profile did not retain its distinct validated fingerprint")
	}
	budget.Profile = outbound.BoundedDocumentsProfile
	budgetFingerprint, err = budget.Fingerprint()
	if err != nil {
		test.Fatal("profile handoff changed the original resource limits")
	}
	if fingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint); err != nil || fingerprint != documents.fingerprint {
		test.Fatal("profile handoff changed the original resource limits")
	}
	api := fixture.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	mcp := fixture.start(test, ctx, outbound.ProcessMCP)
	mcp.expect(test, ctx, "opened", "")
	worker := fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	fixture.assertSessions(test, ctx, 3, 0)
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	fixture.waitDocumentJob(test, ctx)
	completed := fixture.fullHandoffReceipt(test, ctx)
	fixture.publishSnapshot(test, ctx)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	if err := worker.command.Process.Kill(); err != nil {
		test.Fatal("hard kill only the owned running full worker")
	}
	worker.join(test, ctx, true)
	fixture.waitRoleSessionsGone(test, ctx, outbound.ProcessWorker)
	api.waitReady(test, ctx, false)
	mcp.waitReady(test, ctx, false)
	fixture.assertSessions(test, ctx, 2, 0)
	fixture.assertFullHandoffUnavailable(test, ctx, api, mcp)
	recovery := fixture.enqueueFullHandoffDocument(test, ctx)
	queued := recovery.fullHandoffReceipt(test, ctx)
	if queued.State != "waiting" || queued.Attempts != 0 || queued.Version != 0 || queued.DocumentRef() != recovery.job.DocumentRef() {
		test.Fatal("replacement requires useful unclaimed work in the existing document queue")
	}
	worker = fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	fixture.assertSessions(test, ctx, 3, 0)
	recovery.assertFullHandoffReceipt(test, ctx, queued)
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	recovery.waitDocumentJob(test, ctx)
	recovered := recovery.fullHandoffReceipt(test, ctx)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	api.terminateAndAcknowledge(test, ctx, fixture)
	mcp.waitReady(test, ctx, false)
	worker.waitReady(test, ctx, false)
	if err := os.WriteFile(documents.budgetPath, originalManifest, 0600); err != nil {
		test.Fatal("restore only the original documents manifest for the old-profile peer")
	}
	api = documents.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	var roles int
	if err := fixture.observer.QueryRow(ctx, `SELECT count(*) FROM pg_locks locks JOIN pg_stat_activity sessions ON sessions.pid=locks.pid
 WHERE locks.locktype='advisory' AND locks.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND locks.classid::bigint=1129862210 AND locks.objid IN (10,11,12) AND locks.objsubid=2 AND locks.granted
 AND sessions.application_name=ANY($1::text[])`, []string{
		documents.controlName(outbound.ProcessAPI), fixture.controlName(outbound.ProcessMCP), fixture.controlName(outbound.ProcessWorker),
	}).Scan(&roles); err != nil || roles != 3 {
		test.Fatal("mixed readiness proof requires all three actual role locks", roles, err)
	}
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, false)
		child.reject(test, ctx, "mixed-profile", true)
	}
	fixture.assertFullHandoffUnavailable(test, ctx, api, mcp)
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, recovered)
	worker.terminateAndAcknowledge(test, ctx, fixture)
	mcp.terminateAndAcknowledge(test, ctx, fixture)
	api.terminateAndAcknowledge(test, ctx, &documents)
	fixture.assertSessions(test, ctx, 0, 0)

	api = documents.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	mcp = documents.start(test, ctx, outbound.ProcessMCP)
	mcp.expect(test, ctx, "opened", "")
	worker = documents.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	for _, child := range []*executionProcessChild{api, mcp, worker} {
		child.waitReady(test, ctx, true)
	}
	documents.assertSessions(test, ctx, 3, 0)
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	documents.assertDocumentReads(test, ctx, api, mcp)
	documents.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, recovered)
	worker.terminateAndAcknowledge(test, ctx, &documents)
	api.terminateAndAcknowledge(test, ctx, &documents)
	mcp.terminateAndAcknowledge(test, ctx, &documents)
	documents.assertSessions(test, ctx, 0, 0)
	documents.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, recovered)
}

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
