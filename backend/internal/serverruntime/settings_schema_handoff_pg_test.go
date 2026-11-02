//go:build postgres && protocolhandoff && schemahandoff

package serverruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
	"github.com/wnsdy95/cxthub/backend/internal/workerserver"
)

const settingsSchemaHandoffPreviousCatalog = "5ccf1e9041446edf410205a978616aad20acbd3e07dac491fe80185d3a561994"
const settingsSchemaHandoffCurrentCatalog = "ceefc26566f79599aa121c84e5fa128de97a2eb4d057457388dd1cfd3c966db8"
const settingsSchemaHandoffPreviousExecutableSHA256 = "3146224d654cfa753b04d82e42f2692815fb78eb6a457617d41d47a96a57a91a"

type schemaHandoffPredecessor struct {
	count            int
	protocol         int
	catalog          string
	executableSHA256 string
}

func TestFullExecutionProcessesPGSettingsSchemaHandoff151ToCurrent(test *testing.T) {
	runSettingsSchemaHandoff(test, schemaHandoffPredecessor{
		count: 151, protocol: 3, catalog: settingsSchemaHandoffPreviousCatalog, executableSHA256: settingsSchemaHandoffPreviousExecutableSHA256,
	})
}

func TestFullExecutionProcessesPGRawRepresentationSchemaHandoff152To153(test *testing.T) {
	runSettingsSchemaHandoff(test, schemaHandoffPredecessor{
		count: 152, protocol: 3, catalog: "42c721b3d691c2ff3f2b6d717da9a879246fe031ae464885a317c3ec14937f5b",
		executableSHA256: "214a91a58b3c3b01a346bbb05463de27f8506f5e7febf952525dfba339699758",
	})
}

func TestFullExecutionProcessesPGNormalizationSchemaHandoff153To158(test *testing.T) {
	runSettingsSchemaHandoff(test, schemaHandoffPredecessor{
		count: 153, protocol: 5, catalog: "41513dcfe9bd29d89f5bf54bfef1a9d8efe49a1ebc0848b6eafedae779df6a5a",
		executableSHA256: "860d7afe5acf9b0f906c437b0d331903c13e1b45f53b392d76e155cb2e7a24a5",
	})
}

func runSettingsSchemaHandoff(test *testing.T, predecessor schemaHandoffPredecessor) {
	test.Helper()
	previousExecutable := releaseProtocolPreviousExecutable(test)
	if os.Getenv("CXT_TEST_PREVIOUS_RUNTIME_SHA256") != predecessor.executableSHA256 {
		test.Fatal("schemahandoff requires the exact source-qualified previous executable", predecessor.count)
	}
	if releaseProtocolPreviousContract(test) != fmt.Sprintf("contract-v%d", predecessor.protocol) {
		test.Fatal("schemahandoff requires the explicit protocol of the source-qualified predecessor", predecessor.protocol, predecessor.count)
	}
	currentExecutable, err := os.Executable()
	if err != nil {
		test.Fatal("locate the actual current runtime executable", err)
	}
	previousInfo, previousErr := os.Stat(previousExecutable)
	currentInfo, currentErr := os.Stat(currentExecutable)
	if previousErr != nil || currentErr != nil || os.SameFile(previousInfo, currentInfo) {
		test.Fatal("schemahandoff requires distinct actual previous-source and current executables")
	}
	if strings.TrimSpace(os.Getenv("CXT_TEST_DSN")) == "" {
		test.Fatal("schemahandoff requires CXT_TEST_DSN; an unexecuted schema transition cannot be skipped into qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	previousBundle := settingsSchemaHandoffBundle(test, os.Getenv("CXT_TEST_PREVIOUS_RUNTIME_MIGRATIONS"), predecessor.count, predecessor.catalog)
	currentSource, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		test.Fatal("resolve the complete current released SQL bundle", err)
	}
	currentBundle := settingsSchemaHandoffBundle(test, currentSource, 158, settingsSchemaHandoffCurrentCatalog)
	entries, err := os.ReadDir(previousBundle)
	if err != nil {
		test.Fatal("read the copied historical bundle", err)
	}
	for _, entry := range entries {
		previousSQL, previousErr := os.ReadFile(filepath.Join(previousBundle, entry.Name()))
		currentSQL, currentErr := os.ReadFile(filepath.Join(currentBundle, entry.Name()))
		if previousErr != nil || currentErr != nil || !bytes.Equal(previousSQL, currentSQL) {
			test.Fatal("the current bundle must retain every historical SQL file byte-for-byte", entry.Name())
		}
	}
	fixture := newExecutionProcessFixtureWithMigrations(test, ctx, previousBundle)
	fixture.activateFullProfile(test)
	manifest, err := os.ReadFile(fixture.budgetPath)
	if err != nil {
		test.Fatal("retain the unchanged full-worker budget", err)
	}
	budget, err := executionprofile.Parse(manifest)
	if err != nil || budget.Profile != outbound.BoundedWorkersProfile {
		test.Fatal("schema transition requires the existing validated full-worker profile", err)
	}
	budgetFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal("retain the same raw budget identity across schemas", err)
	}
	previous := *fixture
	previous.fingerprint = fixture.releaseProtocolPreviousFingerprint(test, ctx, budgetFingerprint)
	current := *fixture
	current.migrations = currentBundle
	for _, release := range []struct {
		owner    *executionProcessFixture
		catalog  string
		protocol int
	}{
		{owner: &previous, catalog: predecessor.catalog, protocol: predecessor.protocol},
		{owner: &current, catalog: settingsSchemaHandoffCurrentCatalog, protocol: 7},
	} {
		identity := sha256.Sum256([]byte(fmt.Sprintf("cxthub/process-admission/v%d\x00%s\x00%s", release.protocol, release.catalog, budgetFingerprint)))
		if release.owner.fingerprint != base64.RawURLEncoding.EncodeToString(identity[:]) {
			test.Fatal("each executable must retain its own protocol and compiled catalog under the same raw budget")
		}
	}
	if previous.fingerprint == current.fingerprint || previous.migrations == current.migrations {
		test.Fatal("schema transition must not alias the old/current release identities or SQL roots")
	}
	assertBudget := func() {
		retained, err := os.ReadFile(fixture.budgetPath)
		if err != nil || !bytes.Equal(retained, manifest) {
			test.Fatal("schema transition changed the original full-worker budget bytes", err)
		}
	}
	startCohort := func(owner *executionProcessFixture, executable string) []*executionProcessChild {
		stable := fixture.settingsSchemaHandoffStableState(test, ctx)
		children := make([]*executionProcessChild, 0, 3)
		for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
			child := owner.startExecutable(test, ctx, role, executable)
			child.expect(test, ctx, "opened", "")
			if fixture.settingsSchemaHandoffStableState(test, ctx) != stable {
				test.Fatal("successful root startup changed stable schema, history, settings, quota or original document receipts", role)
			}
			children = append(children, child)
		}
		for _, child := range children {
			child.waitReady(test, ctx, true)
		}
		owner.assertSessions(test, ctx, 3, 0)
		fixture.assertReleaseProtocolRoster(test, ctx, owner, owner, owner)
		return children
	}
	drain := func(owner *executionProcessFixture, children []*executionProcessChild) {
		for index := len(children) - 1; index >= 0; index-- {
			children[index].terminateAndAcknowledge(test, ctx, owner)
		}
		owner.assertSessions(test, ctx, 0, 0)
		assertBudget()
	}
	assertCurrentRefused := func(admitted *executionProcessFixture) {
		before := fixture.settingsSchemaHandoffState(test, ctx)
		for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
			child := current.startExecutableChild(test, ctx, role, currentExecutable, "TestSettingsSchemaHandoffPGRefusalChild")
			if event := child.expect(test, ctx, "refused", ""); event.Code != "schema-mismatch" {
				test.Fatal("the current root on the historical schema must refuse specifically with SchemaMismatch, not conflict or a generic startup error", role, event.Code)
			}
			child.join(test, ctx, false)
			if fixture.settingsSchemaHandoffState(test, ctx) != before {
				test.Fatal("refused root changed schema, history, ownership, settings, quota, useful data or process admission", role)
			}
			if admitted == nil {
				fixture.assertSessions(test, ctx, 0, 0)
			} else {
				admitted.assertSessions(test, ctx, 3, 0)
				fixture.assertReleaseProtocolRoster(test, ctx, admitted, admitted, admitted)
			}
			assertBudget()
		}
	}
	children := startCohort(&previous, previousExecutable)
	api, mcp, worker := children[0], children[1], children[2]
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	fixture.waitDocumentJob(test, ctx)
	completed := fixture.fullHandoffReceipt(test, ctx)
	if completed.Attempts != 1 || completed.Version != 1 {
		test.Fatal("actual old full worker did not complete exactly its original document claim")
	}
	fixture.publishSnapshot(test, ctx)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	legacySettings := []domain.SettingsBundle{
		{Kind: "claude", Files: []domain.SettingsFile{{Path: "CLAUDE.md", ContentB64: base64.StdEncoding.EncodeToString([]byte("historical Claude settings \ud55c🙂"))}}},
		{Kind: "agents", Files: []domain.SettingsFile{{Path: "AGENTS.md", ContentB64: base64.StdEncoding.EncodeToString([]byte("historical Agents settings \ud55c🙂"))}}},
	}
	for _, bundle := range legacySettings {
		fixture.settingsSchemaHandoffSettings(test, ctx, api, bundle, true)
	}
	retainedSettings := fixture.settingsSchemaHandoffLegacySettings(test, ctx)
	worker.terminateAndAcknowledge(test, ctx, &previous)
	worker = previous.startExecutable(test, ctx, outbound.ProcessWorker, previousExecutable)
	worker.expect(test, ctx, "opened", "")
	children[2] = worker
	for _, child := range children {
		child.waitReady(test, ctx, true)
	}
	assertCurrentRefused(&previous)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	drain(&previous, children)
	recovery := fixture.enqueueFullHandoffDocument(test, ctx)
	queued := recovery.fullHandoffReceipt(test, ctx)
	if queued.State != "waiting" || queued.Attempts != 0 || queued.Version != 0 || queued.ID == completed.ID || queued.DocumentRef() != recovery.job.DocumentRef() {
		test.Fatal("schema migration requires distinct useful work retaining its original unclaimed receipt")
	}
	assertCurrentRefused(nil)
	preserved := fixture.settingsSchemaHandoffPreservedData(test, ctx, predecessor.count)
	if applied, err := fixture.storage.ApplyPublicMigrations(ctx, current.migrations); err != nil || applied != 158-predecessor.count {
		test.Fatal("only an explicit operator application of the complete current bundle may apply the exact missing migrations", applied, err)
	}
	history := fixture.releaseProtocolCatalog(test, ctx)
	if fixture.settingsSchemaHandoffPreservedData(test, ctx, predecessor.count) != preserved {
		test.Fatal("the schema delta changed historical checksums/applied_at, ownership, settings, quota, documents or durable receipts")
	}
	if err := fixture.storage.VerifyMigrations(ctx, current.migrations); err != nil {
		test.Fatal("the operator-migrated schema must satisfy the current compiled catalog and settings constraint verification", err)
	}
	beforeReapply := fixture.settingsSchemaHandoffState(test, ctx)
	if applied, err := fixture.storage.ApplyPublicMigrations(ctx, current.migrations); err != nil || applied != 0 {
		test.Fatal("reapplying the complete current bundle must be an unchanged zero-delta operation", applied, err)
	}
	if fixture.settingsSchemaHandoffState(test, ctx) != beforeReapply {
		test.Fatal("zero-delta reapplication repaired or rewrote retained state")
	}
	previousRefusalState := fixture.settingsSchemaHandoffState(test, ctx)
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		child := previous.startExecutable(test, ctx, role, previousExecutable)
		if event := child.expect(test, ctx, "refused", ""); event.Code != "unexpected" {
			test.Fatal("the exact historical root must retain its original classifier's coarse startup refusal on the current schema", role, event.Code)
		}
		child.join(test, ctx, false)
		fixture.assertSessions(test, ctx, 0, 0)
		if fixture.settingsSchemaHandoffState(test, ctx) != previousRefusalState {
			test.Fatal("exact historical refusal changed schema, history, ownership, settings, quota, useful data, receipts or admission", role)
		}
		assertBudget()
	}
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, queued)
	test.Logf("the same exact historical binary/catalog %d served useful work and refused all three roots after the preserved-data 158 migration; its original classifier exposes only coarse unexpected, not directly observed typed SchemaMismatch", predecessor.count)
	beforeDowngrade := fixture.settingsSchemaHandoffState(test, ctx)
	if applied, err := fixture.storage.ApplyPublicMigrations(ctx, previous.migrations); applied != 0 || !errors.Is(err, store.ErrSchemaMismatch) {
		test.Fatal("cross-schema downgrade is unsupported; the historical bundle must not reverse or repair the current database", applied, err)
	}
	if fixture.settingsSchemaHandoffState(test, ctx) != beforeDowngrade {
		test.Fatal("rejected cross-schema downgrade changed the current catalog or retained data")
	}
	children = startCohort(&current, currentExecutable)
	api, mcp, worker = children[0], children[1], children[2]
	fixture.assertDocumentReads(test, ctx, api, mcp)
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, queued)
	for _, bundle := range legacySettings {
		fixture.settingsSchemaHandoffSettings(test, ctx, api, bundle, false)
	}
	codex := domain.SettingsBundle{Kind: "codex", Files: []domain.SettingsFile{{Path: "config.toml", ContentB64: base64.StdEncoding.EncodeToString([]byte("model = \"fixture-codex\"\n"))}}}
	fixture.settingsSchemaHandoffSettings(test, ctx, api, codex, true)
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	recovery.waitDocumentJob(test, ctx)
	recovered := recovery.fullHandoffReceipt(test, ctx)
	if recovered.Attempts != 1 || recovered.Version != queued.Version+1 {
		test.Fatal("current full worker lost or duplicated the original queued receipt across the schema migration")
	}
	if err := fixture.storage.PutSnapshot(executionProcessSystemContext(ctx), domain.Snapshot{
		ID: recovery.job.DocHash, RepoID: fixture.repo, DocHash: recovery.job.DocHash, DocIdentity: domain.DocumentIdentityRootV1,
		Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC(),
	}); err != nil {
		test.Fatal("publish the recovered snapshot only after its actual current full-worker receipt", err)
	}
	recovery.assertDocumentReads(test, ctx, api, mcp)
	fixture.assertDocumentReads(test, ctx, api, mcp)
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	var exactReceipts bool
	if err := fixture.observer.QueryRow(ctx, `SELECT count(*)=2
 AND count(*) FILTER (WHERE id=ANY($2::text[]) AND state='completed')=2
 FROM public.doc_finalization_jobs WHERE repo_id=$1`, fixture.repo, []string{completed.ID, recovered.ID}).Scan(&exactReceipts); err != nil || !exactReceipts {
		test.Fatal("schema transition lost or duplicated an original durable document receipt", err)
	}
	drain(&current, children)
	restartState := fixture.settingsSchemaHandoffState(test, ctx)
	children = startCohort(&current, currentExecutable)
	api, mcp = children[0], children[1]
	children[2].send(test, executionProcessCommand{Operation: "run"})
	children[2].expect(test, ctx, "running", "")
	fixture.assertDocumentReads(test, ctx, api, mcp)
	recovery.assertDocumentReads(test, ctx, api, mcp)
	for _, bundle := range append(legacySettings, codex) {
		fixture.settingsSchemaHandoffSettings(test, ctx, api, bundle, false)
	}
	fixture.assertFullHandoffReceipt(test, ctx, completed)
	recovery.assertFullHandoffReceipt(test, ctx, recovered)
	drain(&current, children)
	if fixture.settingsSchemaHandoffState(test, ctx) != restartState || fixture.releaseProtocolCatalog(test, ctx) != history || fixture.settingsSchemaHandoffLegacySettings(test, ctx) != retainedSettings {
		test.Fatal("current three-root restart changed schema/history, settings data/hash, quota or useful document receipts")
	}
	test.Logf("%d-to-158 schema and protocol%d-to-7 upgrade requires a complete drain and explicit migration; historical roots refuse schema158, and same-catalog rollback is separate qualification", predecessor.count, predecessor.protocol)
}

func settingsSchemaHandoffBundle(test *testing.T, source string, count int, expected string) string {
	test.Helper()
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		test.Fatal("schemahandoff requires an absolute clean SQL directory; set CXT_TEST_PREVIOUS_RUNTIME_MIGRATIONS to the complete qualified historical bundle")
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil || resolved != source {
		test.Fatal("schemahandoff SQL source must not traverse symlinks", err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		test.Fatal("read the explicitly selected released SQL bundle", err)
	}
	directory := test.TempDir()
	catalog := sha256.New()
	copied, latest := 0, ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if !entry.Type().IsRegular() {
			test.Fatal("schemahandoff requires regular historical/current SQL files", entry.Name())
		}
		raw, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			test.Fatal("read exact released SQL bytes", entry.Name(), err)
		}
		checksum := sha256.Sum256(raw)
		if _, err := fmt.Fprintf(catalog, "%s\x00%x\n", entry.Name(), checksum); err != nil {
			test.Fatal("hash the complete released SQL catalog", err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), raw, 0600); err != nil {
			test.Fatal("copy released SQL only into the private test bundle", err)
		}
		copied++
		latest = entry.Name()
	}
	wantLatest := "0151_legacy_byte_header_accounting.sql"
	if count == 152 {
		wantLatest = "0152_settings_codex.sql"
	} else if count == 153 {
		wantLatest = "0153_blob_representation_quota.sql"
	} else if count == 158 {
		wantLatest = "0158_legacy_normalization_stage.sql"
	}
	if copied != count || latest != wantLatest || hex.EncodeToString(catalog.Sum(nil)) != expected {
		test.Fatal("the selected SQL bundle is not the complete exact historical/current released catalog", copied, latest)
	}
	return directory
}

func TestSettingsSchemaHandoffPGRefusalChild(test *testing.T) {
	role := outbound.ProcessRole(os.Getenv("CXT_WAVE39_PROCESS_ROLE"))
	if role == "" {
		test.Skip("entered only by the isolated schema handoff refusal proof")
	}
	configuration := executionProcessLocalConfig(test, os.Getenv("CXT_POSTGRES_DSN"))
	database := os.Getenv("CXT_WAVE39_PROCESS_DATABASE")
	if !strings.HasPrefix(database, "bounded_process_") || configuration.ConnConfig.Database != database || os.Getenv("CXT_EXECUTION_PROFILE") != outbound.BoundedWorkersProfile {
		test.Fatal("schema refusal child requires the parent's explicit owned local full-profile database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	var root *serverruntime.Runtime
	var worker *serverruntime.WorkerRuntime
	var err error
	switch role {
	case outbound.ProcessAPI:
		root, err = serverruntime.Open(ctx, "127.0.0.1:0", os.Getenv("HOME"), true)
	case outbound.ProcessMCP:
		root, err = serverruntime.OpenMCP(ctx, "127.0.0.1:0", os.Getenv("HOME"), true)
	case outbound.ProcessWorker:
		worker, err = workerserver.OpenWorker(ctx)
	default:
		test.Fatal("invalid schema refusal child role")
	}
	if root != nil {
		root.Close()
	}
	if worker != nil {
		_ = worker.Close()
	}
	if root != nil || worker != nil || !errors.Is(err, store.ErrSchemaMismatch) || !strings.HasPrefix(err.Error(), "verify schema (run cxt-admin migrate --apply before serving): ") {
		test.Fatal("actual cross-schema root did not refuse specifically with SchemaMismatch before admission", err)
	}
	if runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory {
		test.Fatal("schema-refused root retained process runtime settings")
	}
	event := executionProcessEvent{State: "refused", Code: "schema-mismatch", PID: os.Getpid()}
	encoded, err := json.Marshal(event)
	if err != nil {
		test.Fatal("encode the typed schema refusal evidence", err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "CXT_W39 %s\n", encoded); err != nil {
		test.Fatal("emit the typed schema refusal evidence", err)
	}
}

func (fixture *executionProcessFixture) settingsSchemaHandoffSettings(test *testing.T, ctx context.Context, api *executionProcessChild, bundle domain.SettingsBundle, write bool) {
	test.Helper()
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil || domain.ValidateSettingsBundle(bundle.Kind, hash, bundle) != nil {
		test.Fatal("construct an unchanged valid content-addressed settings bundle", bundle.Kind, err)
	}
	client := &http.Client{
		Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	encoded, err := json.Marshal(bundle)
	if err != nil {
		test.Fatal("encode actual API settings input", err)
	}
	for _, suffix := range []string{"/settings/" + bundle.Kind, "/settings-objects/" + string(hash)} {
		methods := []string{http.MethodGet}
		if write {
			methods = []string{http.MethodPut, http.MethodGet}
		}
		for _, method := range methods {
			request, err := http.NewRequestWithContext(ctx, method, api.url+"/api/v1/repos/"+string(fixture.repo)+suffix, bytes.NewReader(encoded))
			if err != nil {
				test.Fatal("construct actual authorized settings request", err)
			}
			request.Header.Set("Authorization", "Bearer "+fixture.session)
			request.Header.Set("X-Cxt-Doc-Identities", string(domain.DocumentIdentityRootV1))
			request.Header.Set("Content-Type", "application/json")
			status, body := executionProcessHTTP(test, client, request)
			if status != http.StatusOK {
				test.Fatal("actual API failed useful settings storage/read across the schema boundary", bundle.Kind, method, suffix, status)
			}
			if method == http.MethodGet {
				var retained domain.SettingsBundle
				if err := json.Unmarshal(body, &retained); err != nil || !reflect.DeepEqual(retained.Files, bundle.Files) || domain.ValidateSettingsBundle(bundle.Kind, hash, retained) != nil {
					test.Fatal("actual API rewrote settings bytes, kind or content-addressed hash", bundle.Kind, suffix, err)
				}
			}
		}
	}
}

func (fixture *executionProcessFixture) settingsSchemaHandoffLegacySettings(test *testing.T, ctx context.Context) string {
	test.Helper()
	var retained string
	if err := fixture.observer.QueryRow(ctx, `SELECT jsonb_build_object(
 'settings',(SELECT jsonb_agg(to_jsonb(settings) ORDER BY repo_id,kind) FROM public.repo_settings settings WHERE kind IN ('claude','agents')),
 'objects',(SELECT jsonb_agg(to_jsonb(objects) ORDER BY repo_id,hash) FROM public.settings_objects objects WHERE data->>'kind' IN ('claude','agents'))
 )::text`).Scan(&retained); err != nil {
		test.Fatal("retain exact historical Claude/Agents settings data and object hashes", err)
	}
	return retained
}

func (fixture *executionProcessFixture) settingsSchemaHandoffPreservedData(test *testing.T, ctx context.Context, count int) string {
	test.Helper()
	var retained string
	var historical int
	upper := fmt.Sprintf("%04d_", count+1)
	if err := fixture.observer.QueryRow(ctx, `SELECT count(*) FROM public.schema_migrations
 WHERE version<$1`, upper).Scan(&historical); err != nil || historical != count {
		test.Fatal("schema transition must preserve every historical receipt", historical, err)
	}
	if err := fixture.observer.QueryRow(ctx, `SELECT jsonb_build_object(
 'documents',$1::jsonb,
 'historical',(SELECT jsonb_agg(to_jsonb(history) ORDER BY version) FROM public.schema_migrations history WHERE version<$2),
 'ownership',(SELECT jsonb_agg(to_jsonb(ownership) ORDER BY version) FROM public.ownership_migrations ownership),
 'settings',(SELECT jsonb_agg(to_jsonb(settings) ORDER BY repo_id,kind) FROM public.repo_settings settings),
 'objects',(SELECT jsonb_agg(to_jsonb(objects) ORDER BY repo_id,hash) FROM public.settings_objects objects),
 'quota',(SELECT jsonb_agg(to_jsonb(accounts) ORDER BY namespace_id) FROM public.storage_accounts accounts),
 'quota_objects',(SELECT jsonb_agg(to_jsonb(objects) ORDER BY namespace_id,object_key) FROM public.storage_usage_objects objects),
 'ledger',(SELECT jsonb_agg(to_jsonb(ledger) ORDER BY seq) FROM public.storage_usage_ledger ledger),
 'policy',(SELECT jsonb_agg(to_jsonb(operations) ORDER BY id) FROM public.storage_policy_operations operations),
 'legacy_usage',(SELECT jsonb_agg(to_jsonb(usage) ORDER BY doc_hash) FROM public.doc_legacy_byte_usage_v1 usage)
 )::text`, fixture.releaseProtocolData(test, ctx), upper).Scan(&retained); err != nil {
		test.Fatal("capture original historical checksums/applied_at, ownership, settings, quota ledger and useful document receipts", err)
	}
	return retained
}

func (fixture *executionProcessFixture) settingsSchemaHandoffStableState(test *testing.T, ctx context.Context) string {
	test.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture.settingsSchemaHandoffState(test, ctx)), &state); err != nil {
		test.Fatal("decode the observed schema and data image", err)
	}
	delete(state, "controls")
	delete(state, "admission")
	encoded, err := json.Marshal(state)
	if err != nil {
		test.Fatal("retain the stable schema and data image without live process locks", err)
	}
	return string(encoded)
}

func (fixture *executionProcessFixture) settingsSchemaHandoffState(test *testing.T, ctx context.Context) string {
	test.Helper()
	var state string
	if err := fixture.observer.QueryRow(ctx, `SELECT jsonb_build_object(
 'data',$1::jsonb,
 'history',(SELECT jsonb_agg(to_jsonb(history) ORDER BY version) FROM public.schema_migrations history),
 'controls',(SELECT jsonb_agg(jsonb_build_array(pid,application_name) ORDER BY pid) FROM pg_stat_activity
  WHERE datname=current_database() AND application_name LIKE 'cxthub/bd1/%'),
 'admission',(SELECT jsonb_agg(jsonb_build_array(pid,classid::bigint,objid::bigint,objsubid,mode,granted) ORDER BY pid,classid,objid,objsubid,mode,granted)
  FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=1129862210),
 'schema',(SELECT COALESCE(jsonb_object_agg(kind||':'||identity,definition),'{}'::jsonb) FROM (
  SELECT 'relation' AS kind,relname::text AS identity,
   jsonb_build_array(oid,relfilenode,relkind,relpersistence,relowner,relacl,relrowsecurity,relforcerowsecurity,
    CASE WHEN relkind IN ('v','m') THEN pg_get_viewdef(oid) END)::text AS definition
  FROM pg_class WHERE relnamespace='public'::regnamespace AND relkind IN ('r','p','v','m','S','f')
  UNION ALL
  SELECT 'column',relation.relname||'.'||attribute.attnum,
   jsonb_build_array(attribute.attname,format_type(attribute.atttypid,attribute.atttypmod),attribute.attnotnull,
    attribute.attidentity,attribute.attgenerated,pg_get_expr(defaults.adbin,defaults.adrelid))::text
  FROM pg_attribute attribute JOIN pg_class relation ON relation.oid=attribute.attrelid
  LEFT JOIN pg_attrdef defaults ON defaults.adrelid=attribute.attrelid AND defaults.adnum=attribute.attnum
  WHERE relation.relnamespace='public'::regnamespace AND attribute.attnum>0 AND NOT attribute.attisdropped
  UNION ALL
  SELECT 'constraint',relation.relname||'.'||constraint_row.conname,
   jsonb_build_array(constraint_row.oid,pg_get_constraintdef(constraint_row.oid),constraint_row.convalidated,
    constraint_row.conislocal,constraint_row.conkey)::text
  FROM pg_constraint constraint_row JOIN pg_class relation ON relation.oid=constraint_row.conrelid
  WHERE relation.relnamespace='public'::regnamespace
  UNION ALL
  SELECT 'index',indexrelid::regclass::text,jsonb_build_array(pg_get_indexdef(indexrelid),indisvalid,indisready)::text
  FROM pg_index JOIN pg_class relation ON relation.oid=indrelid WHERE relation.relnamespace='public'::regnamespace
  UNION ALL
  SELECT 'function',oid::regprocedure::text,jsonb_build_array(oid,proowner,proacl,pg_get_functiondef(oid))::text
  FROM pg_proc WHERE pronamespace='public'::regnamespace AND prokind IN ('f','p')
  UNION ALL
  SELECT 'trigger',relation.relname||'.'||trigger_row.tgname,
   jsonb_build_array(pg_get_triggerdef(trigger_row.oid),trigger_row.tgenabled)::text
  FROM pg_trigger trigger_row JOIN pg_class relation ON relation.oid=trigger_row.tgrelid
  WHERE relation.relnamespace='public'::regnamespace AND NOT trigger_row.tgisinternal
  UNION ALL
  SELECT 'policy',relation.relname||'.'||policy.polname,to_jsonb(policy)::text
  FROM pg_policy policy JOIN pg_class relation ON relation.oid=policy.polrelid
  WHERE relation.relnamespace='public'::regnamespace
 ) observed_schema)
 )::text`, fixture.settingsSchemaHandoffPreservedData(test, ctx, 151)).Scan(&state); err != nil {
		test.Fatal("observe exact schema, receipts and admission without DDL, repair or history normalization", err)
	}
	return state
}
