//go:build postgres && protocolhandoff

package serverruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestFullExecutionProcessesPGExactReleaseProtocolHandoff(test *testing.T) {
	previousExecutable := releaseProtocolPreviousExecutable(test)
	currentExecutable, err := os.Executable()
	if err != nil {
		test.Fatal("locate the actual current runtime test executable", err)
	}
	previousInfo, previousErr := os.Stat(previousExecutable)
	currentInfo, currentErr := os.Stat(currentExecutable)
	if previousErr != nil || currentErr != nil || os.SameFile(previousInfo, currentInfo) {
		test.Fatal("the exact previous-source executable must be distinct from the current executable")
	}
	if strings.TrimSpace(os.Getenv("CXT_TEST_DSN")) == "" {
		test.Fatal("protocolhandoff requires CXT_TEST_DSN for the existing owned local PostgreSQL fixture")
	}
	for _, direction := range []struct {
		name        string
		legacyFirst bool
	}{
		{name: "upgrade-exact-previous-to-current", legacyFirst: true},
		{name: "rollback-current-to-exact-previous-same-catalog-and-budget", legacyFirst: false},
	} {
		test.Run(direction.name, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fixture := newExecutionProcessFixture(test, ctx)
			fixture.activateFullProfile(test)
			manifest, err := os.ReadFile(fixture.budgetPath)
			if err != nil {
				test.Fatal("retain the unchanged full-profile budget", err)
			}
			budget, err := executionprofile.Parse(manifest)
			if err != nil || budget.Profile != outbound.BoundedWorkersProfile {
				test.Fatal("paired releases require the same validated full-worker profile", err)
			}
			budgetFingerprint, err := budget.Fingerprint()
			if err != nil {
				test.Fatal("retain the original raw budget identity", err)
			}
			boundFingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint)
			if err != nil || fixture.fingerprint != boundFingerprint || boundFingerprint == budgetFingerprint {
				test.Fatal("current fixture must witness the release-bound process identity, not the raw budget", err)
			}
			for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
				configuration, err := budget.AdmissionConfig(role)
				if err != nil || configuration.BudgetFingerprint != budgetFingerprint {
					test.Fatal("release binding changed the original role budget configuration", role, err)
				}
			}
			legacy := *fixture
			legacy.fingerprint = fixture.releaseProtocolPreviousFingerprint(test, ctx, budgetFingerprint)
			source, target := fixture, &legacy
			sourceExecutable, targetExecutable := currentExecutable, previousExecutable
			if direction.legacyFirst {
				source, target = &legacy, fixture
				sourceExecutable, targetExecutable = previousExecutable, currentExecutable
			}
			catalog := fixture.releaseProtocolCatalog(test, ctx)
			assertInputs := func() {
				currentManifest, err := os.ReadFile(fixture.budgetPath)
				if err != nil || !bytes.Equal(currentManifest, manifest) {
					test.Fatal("release handoff changed the original full-profile budget bytes", err)
				}
				if fixture.releaseProtocolCatalog(test, ctx) != catalog {
					test.Fatal("exact-source handoff changed the shared released migration receipts")
				}
			}
			startCohort := func(owner *executionProcessFixture, executable string) []*executionProcessChild {
				children := make([]*executionProcessChild, 0, 3)
				for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
					child := owner.startExecutable(test, ctx, role, executable)
					child.expect(test, ctx, "opened", "")
					children = append(children, child)
				}
				for _, child := range children {
					child.waitReady(test, ctx, true)
				}
				owner.assertSessions(test, ctx, 3, 0)
				fixture.assertReleaseProtocolRoster(test, ctx, owner, owner, owner)
				return children
			}
			children := startCohort(source, sourceExecutable)
			api, mcp, worker := children[0], children[1], children[2]
			worker.send(test, executionProcessCommand{Operation: "run"})
			worker.expect(test, ctx, "running", "")
			fixture.waitDocumentJob(test, ctx)
			completed := fixture.fullHandoffReceipt(test, ctx)
			if completed.Attempts != 1 {
				test.Fatal("baseline useful work did not retain exactly one original claim")
			}
			fixture.publishSnapshot(test, ctx)
			fixture.assertDocumentReads(test, ctx, api, mcp)
			assertInputs()

			api.terminateAndAcknowledge(test, ctx, source)
			mcp.waitReady(test, ctx, false)
			worker.waitReady(test, ctx, false)
			api = target.startExecutable(test, ctx, outbound.ProcessAPI, targetExecutable)
			api.expect(test, ctx, "opened", "")
			fixture.assertReleaseProtocolRoster(test, ctx, target, source, source)
			children = []*executionProcessChild{api, mcp, worker}
			for _, child := range children {
				child.waitReady(test, ctx, false)
			}
			recovery := fixture.enqueueFullHandoffDocument(test, ctx)
			queued := recovery.fullHandoffReceipt(test, ctx)
			if queued.State != "waiting" || queued.Attempts != 0 || queued.Version != 0 || queued.ID == completed.ID || queued.DocumentRef() != recovery.job.DocumentRef() {
				test.Fatal("mixed releases require distinct useful work still awaiting its original claim")
			}
			mixedData := fixture.releaseProtocolData(test, ctx)
			for _, child := range children {
				child.reject(test, ctx, "mixed-release-interactive", false)
				child.reject(test, ctx, "mixed-release-bulk", true)
			}
			fixture.assertFullHandoffUnavailable(test, ctx, api, mcp)
			for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
				duplicate := target.startExecutable(test, ctx, role, targetExecutable)
				if event := duplicate.expect(test, ctx, "refused", ""); event.Code != "conflict" {
					test.Fatal("release identity bypassed the unchanged actual role lock", role, event.Code)
				}
				duplicate.join(test, ctx, false)
			}
			fixture.assertReleaseProtocolRoster(test, ctx, target, source, source)
			for _, child := range children {
				child.assertReady(test, ctx, false)
			}
			fixture.assertFullHandoffReceipt(test, ctx, completed)
			recovery.assertFullHandoffReceipt(test, ctx, queued)
			if fixture.releaseProtocolData(test, ctx) != mixedData {
				test.Fatal("mixed release admission or protocol rejection changed durable document data or receipts")
			}
			assertInputs()

			worker.terminateAndAcknowledge(test, ctx, source)
			mcp.terminateAndAcknowledge(test, ctx, source)
			api.terminateAndAcknowledge(test, ctx, target)
			fixture.assertSessions(test, ctx, 0, 0)
			if fixture.releaseProtocolData(test, ctx) != mixedData {
				test.Fatal("draining the incompatible cohort changed useful data or receipts")
			}
			children = startCohort(target, targetExecutable)
			api, mcp, worker = children[0], children[1], children[2]
			fixture.assertDocumentReads(test, ctx, api, mcp)
			fixture.assertFullHandoffReceipt(test, ctx, completed)
			recovery.assertFullHandoffReceipt(test, ctx, queued)
			if fixture.releaseProtocolData(test, ctx) != mixedData {
				test.Fatal("starting the complete target cohort rewrote data before worker execution")
			}
			worker.send(test, executionProcessCommand{Operation: "run"})
			worker.expect(test, ctx, "running", "")
			recovery.waitDocumentJob(test, ctx)
			recovered := recovery.fullHandoffReceipt(test, ctx)
			if recovered.Attempts != 1 || recovered.Version != queued.Version+1 {
				test.Fatal("target worker lost or duplicated the waiting receipt's original claim")
			}
			var exactReceipts bool
			if err := fixture.observer.QueryRow(ctx, `SELECT count(*)=2
 AND count(*) FILTER (WHERE id=ANY($2::text[]) AND state='completed')=2
 FROM public.doc_finalization_jobs WHERE repo_id=$1`, fixture.repo, []string{completed.ID, recovered.ID}).Scan(&exactReceipts); err != nil || !exactReceipts {
				test.Fatal("handoff lost or duplicated an original durable receipt", err)
			}
			fixture.assertReleaseProtocolRoster(test, ctx, target, target, target)
			fixture.assertDocumentReads(test, ctx, api, mcp)
			fixture.assertFullHandoffReceipt(test, ctx, completed)
			targetData := fixture.releaseProtocolData(test, ctx)
			worker.terminateAndAcknowledge(test, ctx, target)
			api.terminateAndAcknowledge(test, ctx, target)
			mcp.terminateAndAcknowledge(test, ctx, target)
			target.assertSessions(test, ctx, 0, 0)
			fixture.assertFullHandoffReceipt(test, ctx, completed)
			recovery.assertFullHandoffReceipt(test, ctx, recovered)
			if fixture.releaseProtocolData(test, ctx) != targetData {
				test.Fatal("target cohort shutdown changed completed useful data or receipts")
			}
			assertInputs()
		})
	}
}

func releaseProtocolPreviousExecutable(test *testing.T) string {
	test.Helper()
	path := os.Getenv("CXT_TEST_PREVIOUS_RUNTIME_EXECUTABLE")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_EXECUTABLE must be an absolute clean path to the exact qualified previous-source test executable")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_EXECUTABLE must be a regular executable, not a symlink", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_EXECUTABLE must not traverse symlinks", err)
	}
	expected := os.Getenv("CXT_TEST_PREVIOUS_RUNTIME_SHA256")
	digest, err := hex.DecodeString(expected)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != expected {
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_SHA256 must be the canonical lowercase SHA256 of the exact previous-source executable")
	}
	binary, err := os.Open(path)
	if err != nil {
		test.Fatal("open the exact previous-source test executable for hashing", err)
	}
	defer binary.Close()
	opened, err := binary.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		test.Fatal("previous-source executable changed before hashing", err)
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, binary)
	if err != nil || written != opened.Size() || !bytes.Equal(hasher.Sum(nil), digest) {
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_SHA256 does not match the actual previous-source executable", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		test.Fatal("previous-source executable changed while hashing", err)
	}
	test.Logf("paired predecessor contract=%s executable_sha256=%s; source/build provenance and rollback qualification apply only to this exact executable, schema catalog and budget", releaseProtocolPreviousContract(test), expected)
	return path
}

func TestReleaseProtocolPreviousContractVersions(test *testing.T) {
	for _, contract := range []string{"", "raw-budget-v1", "contract-v2", "contract-v3", "contract-v4", "contract-v5", "contract-v6"} {
		test.Run(contract, func(test *testing.T) {
			test.Setenv("CXT_TEST_PREVIOUS_RUNTIME_PROTOCOL", contract)
			expected := contract
			if expected == "" {
				expected = "raw-budget-v1"
			}
			if actual := releaseProtocolPreviousContract(test); actual != expected {
				test.Fatal("predecessor protocol selector changed the declared release contract", actual, expected)
			}
		})
	}
}

func releaseProtocolPreviousContract(test *testing.T) string {
	test.Helper()
	contract := strings.TrimSpace(os.Getenv("CXT_TEST_PREVIOUS_RUNTIME_PROTOCOL"))
	switch contract {
	case "", "raw-budget-v1":
		return "raw-budget-v1"
	case "contract-v2", "contract-v3", "contract-v4", "contract-v5", "contract-v6":
		return contract
	default:
		test.Fatal("CXT_TEST_PREVIOUS_RUNTIME_PROTOCOL must identify raw-budget-v1, contract-v2, contract-v3, contract-v4, contract-v5 or contract-v6")
		return ""
	}
}

func (fixture *executionProcessFixture) releaseProtocolPreviousFingerprint(test *testing.T, ctx context.Context, budgetFingerprint string) string {
	test.Helper()
	protocol := releaseProtocolPreviousContract(test)
	if protocol == "raw-budget-v1" {
		return budgetFingerprint
	}
	version := 2
	if protocol == "contract-v3" {
		version = 3
	} else if protocol == "contract-v4" {
		version = 4
	} else if protocol == "contract-v5" {
		version = 5
	} else if protocol == "contract-v6" {
		version = 6
	}
	rows, err := fixture.observer.Query(ctx, `SELECT version,checksum FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		test.Fatal("read the actual unchanged catalog for the predecessor's contract identity", err)
	}
	defer rows.Close()
	catalog := sha256.New()
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			test.Fatal(err)
		}
		digest, err := hex.DecodeString(checksum)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != checksum {
			test.Fatal("predecessor catalog requires exact canonical released checksums", version, err)
		}
		if _, err := io.WriteString(catalog, version+"\x00"+checksum+"\n"); err != nil {
			test.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	contract := fmt.Sprintf("cxthub/process-admission/v%d\x00%s\x00%s", version, hex.EncodeToString(catalog.Sum(nil)), budgetFingerprint)
	fingerprint := sha256.Sum256([]byte(contract))
	return base64.RawURLEncoding.EncodeToString(fingerprint[:])
}

func (fixture *executionProcessFixture) assertReleaseProtocolRoster(test *testing.T, ctx context.Context, api, mcp, worker *executionProcessFixture) {
	test.Helper()
	expected := map[int64]string{
		10: api.controlName(outbound.ProcessAPI),
		11: mcp.controlName(outbound.ProcessMCP),
		12: worker.controlName(outbound.ProcessWorker),
	}
	rows, err := fixture.observer.Query(ctx, `SELECT locks.objid::bigint,sessions.application_name,count(*),bool_and(locks.mode='ExclusiveLock')
 FROM pg_locks locks JOIN pg_stat_activity sessions ON sessions.pid=locks.pid
 WHERE locks.locktype='advisory' AND locks.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND locks.classid::bigint=1129862210 AND locks.objid IN (10,11,12) AND locks.objsubid=2 AND locks.granted
 AND sessions.datname=current_database() AND sessions.usename=session_user
 GROUP BY locks.objid,sessions.application_name`)
	if err != nil {
		test.Fatal("observe actual paired-release role locks and session identity tags", err)
	}
	defer rows.Close()
	seen := make(map[int64]bool, 3)
	for rows.Next() {
		var role int64
		var tag string
		var count int
		var exclusive bool
		if err := rows.Scan(&role, &tag, &count, &exclusive); err != nil {
			test.Fatal("decode actual paired-release role witnesses", err)
		}
		if count != 1 || !exclusive || seen[role] || expected[role] != tag || len(tag) > 63 {
			test.Fatal("actual paired-release role lock is missing, duplicated or has the wrong identity tag", role, tag, count)
		}
		seen[role] = true
	}
	if err := rows.Err(); err != nil || len(seen) != 3 {
		test.Fatal("paired-release proof requires all three actual role locks with their exact release tags", len(seen), err)
	}
}

func (fixture *executionProcessFixture) releaseProtocolCatalog(test *testing.T, ctx context.Context) string {
	test.Helper()
	var count int
	var latest, history string
	if err := fixture.observer.QueryRow(ctx, `SELECT count(*),max(version),jsonb_agg(to_jsonb(history) ORDER BY version)::text
 FROM public.schema_migrations history`).Scan(&count, &latest, &history); err != nil || count != 158 || latest != "0158_legacy_normalization_stage.sql" {
		test.Fatal("paired releases require the identical completed 158-migration catalog; a cross-schema transition is not a same-catalog rollback", count, latest, err)
	}
	return history
}

func (fixture *executionProcessFixture) releaseProtocolData(test *testing.T, ctx context.Context) string {
	test.Helper()
	var data string
	if err := fixture.observer.QueryRow(ctx, `SELECT jsonb_build_object(
 'objects',(SELECT jsonb_agg(jsonb_build_array(grants.kind,grants.hash,encode(blobs.bytes,'hex')) ORDER BY grants.kind,grants.hash)
 FROM public.repo_blobs grants JOIN public.blobs blobs ON blobs.hash=grants.hash WHERE grants.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(to_jsonb(snapshots) ORDER BY id) FROM public.snapshots snapshots WHERE repo_id=$1),
 'refs',(SELECT jsonb_agg(to_jsonb(refs) ORDER BY kind,name) FROM public.refs refs WHERE repo_id=$1),
 'receipts',(SELECT jsonb_agg(to_jsonb(receipts) ORDER BY id) FROM public.doc_finalization_jobs receipts WHERE repo_id=$1)
 )::text`, fixture.repo).Scan(&data); err != nil {
		test.Fatal("retain exact useful document bytes, grants, snapshots, refs and durable receipts", err)
	}
	return data
}
