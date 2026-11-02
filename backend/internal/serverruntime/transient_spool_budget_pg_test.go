//go:build postgres

package serverruntime

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func executionProfileSpoolEnvironment(test *testing.T, dsn, profileName string) executionprofile.Budget {
	test.Helper()
	wave48WorkerEnvironment(test)
	executionRootPostgresEnvironment(test, dsn)
	budget := fullRuntimeProfileBudget(test, profileName)
	for position := range budget.Roles {
		budget.Roles[position].SpoolBytes = int64(position+1) * (256 << 10)
	}
	test.Setenv("CXT_EXECUTION_PROFILE", profileName)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, budget))
	return budget
}

func executionProfileSpoolCapability(test *testing.T, storage any) outbound.TransientSpoolBudgetStore {
	test.Helper()
	capability, supported := storage.(outbound.TransientSpoolBudgetStore)
	if !supported {
		test.Fatalf("execution store %T lacks transient spool accounting", storage)
	}
	return capability
}

func executionProfileSpoolUsage(test *testing.T, capability outbound.TransientSpoolBudgetStore, limit int64) outbound.TransientSpoolBudgetUsage {
	test.Helper()
	usage := capability.TransientSpoolBudgetUsage()
	if usage.LimitBytes != limit || usage.UsedBytes != 0 || usage.OpenFiles != 0 || usage.PeakBytes < 0 ||
		limit > 0 && usage.PeakBytes > limit {
		test.Fatal("execution profile did not apply or release its actual spool budget", usage, limit)
	}
	return usage
}

func executionProfileSpoolDocument(test *testing.T, marker, text string) domain.VerifiedSessionDoc {
	test.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: marker},
		Events: []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		test.Fatal(err)
	}
	return doc
}

func executionProfileSpoolOpen(test *testing.T, ctx context.Context, role outbound.ProcessRole) (store.Store, *executionProfile, func() error) {
	test.Helper()
	if role == outbound.ProcessWorker {
		worker, err := wave48PGOpenWorker(ctx)
		if err != nil {
			test.Fatal("open the actual worker root with a spool budget", err)
		}
		return worker.Store, worker.execution, worker.Close
	}
	open := Open
	if role == outbound.ProcessMCP {
		open = OpenMCP
	}
	root, err := open(ctx, "127.0.0.1:0", "", true)
	if err != nil {
		test.Fatal("open the actual API or MCP root with a spool budget", err)
	}
	return root.Store, root.execution, func() error {
		root.Close()
		return root.infrastructure.Close()
	}
}

func TestExecutionProfileSpoolActualRootsApplyRoleBudgetAndRestore(test *testing.T) {
	for _, profileName := range []string{outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		test.Run(profileName, func(test *testing.T) {
			for _, roleName := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
				test.Run(string(roleName), func(test *testing.T) {
					dsn, _ := executionRootPostgresFixture(test)
					budget := executionProfileSpoolEnvironment(test, dsn, profileName)
					role, err := budget.ForRole(roleName)
					if err != nil {
						test.Fatal(err)
					}
					filesystem := executionProfileSpoolCapability(test, store.NewFSStore(test.TempDir()))
					executionProfileSpoolUsage(test, filesystem, 0)
					previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					test.Cleanup(cancel)
					storage, profile, closeRoot := executionProfileSpoolOpen(test, ctx, roleName)
					test.Cleanup(func() {
						if err := closeRoot(); err != nil {
							test.Error(err)
						}
					})
					if profile.role != role || profile.budget.Profile != profileName {
						test.Fatal("actual composition root changed the selected role or manifest", profile.role, role)
					}
					postgres := executionProfileSpoolCapability(test, storage)
					reads, supported := storage.(outbound.DocReadStore)
					if !supported {
						test.Fatal("actual PostgreSQL root lost its public document read capability")
					}
					executionProfileSpoolUsage(test, postgres, role.SpoolBytes)
					executionProfileSpoolUsage(test, filesystem, role.SpoolBytes)
					repo := domain.HashContent([]byte(test.Name()))
					if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
						test.Fatal(err)
					}
					oversized := executionProfileSpoolDocument(test, test.Name()+"/large", strings.Repeat("Q", int(role.SpoolBytes)+1))
					if created, err := storage.PutVerifiedDoc(ctx, repo, oversized); created || !errors.Is(err, domain.ErrResourceOverloaded) || errors.Is(err, domain.ErrIntegrity) {
						test.Fatal("actual PostgreSQL writes ignored role.SpoolBytes or weakened the error contract", created, err)
					}
					if _, err := storage.GetDoc(ctx, repo, oversized.Hash()); !errors.Is(err, domain.ErrNotFound) {
						test.Fatal("profile budget failure published a document", err)
					}
					executionProfileSpoolUsage(test, postgres, role.SpoolBytes)
					doc := executionProfileSpoolDocument(test, test.Name()+"/small", "Small SearchNeedle \ud55c🙂")
					if created, err := storage.PutVerifiedDoc(ctx, repo, doc); err != nil || !created {
						test.Fatal("actual root did not recover for a small document under its unchanged role cap", created, err)
					}
					got, err := storage.GetDoc(ctx, repo, doc.Hash())
					if err != nil || got.Hash != doc.Hash() {
						test.Fatal("bounded root lost the exact published document identity", err)
					}
					canonical, err := domain.CanonicalBytes(got.CIR)
					if err != nil || !bytes.Equal(canonical, doc.Bytes()) {
						test.Fatal("bounded root changed canonical document bytes", err)
					}
					hits, err := reads.SearchDocEvents(ctx, repo, doc.Hash(), "searchneedle", -1, 1)
					if err != nil || len(hits) != 1 || hits[0].Text != "Small SearchNeedle \ud55c🙂" {
						test.Fatal("bounded root did not publish complete searchable text", hits, err)
					}
					usage := executionProfileSpoolUsage(test, postgres, role.SpoolBytes)
					if usage.PeakBytes == 0 || filesystem.TransientSpoolBudgetUsage() != usage {
						test.Fatal("actual temporary writes were not charged to the process-wide configured role", usage)
					}
					if err := closeRoot(); err != nil {
						test.Fatal("close the actual bounded root", err)
					}
					executionProfileSpoolUsage(test, filesystem, 0)
					if runtime.GOMAXPROCS(0) != previousCPU || debug.SetMemoryLimit(-1) != previousMemory {
						test.Fatal("spool restoration displaced existing runtime restoration")
					}
					if err := closeRoot(); err != nil {
						test.Fatal("bounded root spool cleanup was not idempotent", err)
					}
				})
			}
		})
	}
}

func TestExecutionProfileSpoolAdmissionFailureRollsBackConfiguration(test *testing.T) {
	dsn, _ := executionRootPostgresFixture(test)
	budget := executionProfileSpoolEnvironment(test, dsn, outbound.BoundedDocumentsProfile)
	role, err := budget.ForRole(outbound.ProcessAPI)
	if err != nil {
		test.Fatal(err)
	}
	dataDSN, err := executionDataDSN(dsn, role)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	test.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dataDSN)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	configuration, err := budget.AdmissionConfig(outbound.ProcessAPI)
	if err != nil {
		test.Fatal(err)
	}
	held, err := storage.OpenProcessAdmission(ctx, configuration)
	if err != nil {
		test.Fatal("reserve only the owned API role to force startup rejection", err)
	}
	test.Cleanup(func() { _ = held.Close() })
	capability := executionProfileSpoolCapability(test, storage)
	executionProfileSpoolUsage(test, capability, 0)
	previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	root, err := Open(ctx, "127.0.0.1:0", "", true)
	if root != nil {
		root.Close()
		test.Fatal("colliding API role unexpectedly opened a bounded root")
	}
	if err == nil {
		test.Fatal("bounded startup ignored the occupied process-admission role")
	}
	executionProfileSpoolUsage(test, capability, 0)
	if runtime.GOMAXPROCS(0) != previousCPU || debug.SetMemoryLimit(-1) != previousMemory {
		test.Fatal("failed admission changed process runtime limits")
	}
	if err := held.Close(); err != nil {
		test.Fatal(err)
	}
	_, _, closeRoot := executionProfileSpoolOpen(test, ctx, outbound.ProcessAPI)
	test.Cleanup(func() { _ = closeRoot() })
	executionProfileSpoolUsage(test, capability, role.SpoolBytes)
	if err := closeRoot(); err != nil {
		test.Fatal("spool configuration could not recover after admission cleanup", err)
	}
	executionProfileSpoolUsage(test, capability, 0)
}

func TestExecutionProfileSpoolWorkerStartupFailureRestoresConfiguration(test *testing.T) {
	dsn, _ := executionRootPostgresFixture(test)
	budget := executionProfileSpoolEnvironment(test, dsn, outbound.BoundedWorkersProfile)
	role, err := budget.ForRole(outbound.ProcessWorker)
	if err != nil {
		test.Fatal(err)
	}
	filesystem := executionProfileSpoolCapability(test, store.NewFSStore(test.TempDir()))
	executionProfileSpoolUsage(test, filesystem, 0)
	previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	startupErr := errors.New("owned spool worker startup rejection")
	called := false
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	test.Cleanup(cancel)
	worker, err := OpenWorker(ctx, func(storage store.Store) (ApplicationWorkerConfig, error) {
		called = true
		capability := executionProfileSpoolCapability(test, storage)
		executionProfileSpoolUsage(test, capability, role.SpoolBytes)
		executionProfileSpoolUsage(test, filesystem, role.SpoolBytes)
		return ApplicationWorkerConfig{}, startupErr
	})
	if worker != nil {
		_ = worker.Close()
		test.Fatal("worker root survived its provider startup error")
	}
	if !called || !errors.Is(err, startupErr) {
		test.Fatal("startup fixture did not fail after actual infrastructure configuration", called, err)
	}
	executionProfileSpoolUsage(test, filesystem, 0)
	if runtime.GOMAXPROCS(0) != previousCPU || debug.SetMemoryLimit(-1) != previousMemory {
		test.Fatal("failed worker startup did not restore existing process limits")
	}
	_, _, closeRoot := executionProfileSpoolOpen(test, ctx, outbound.ProcessWorker)
	test.Cleanup(func() { _ = closeRoot() })
	executionProfileSpoolUsage(test, filesystem, role.SpoolBytes)
	if err := closeRoot(); err != nil {
		test.Fatal("worker could not reopen and clean up after startup failure", err)
	}
	executionProfileSpoolUsage(test, filesystem, 0)
}

func TestExecutionProfileSpoolRequestDrainPrecedesRestoration(test *testing.T) {
	dsn, _ := executionRootPostgresFixture(test)
	budget := executionProfileSpoolEnvironment(test, dsn, outbound.BoundedDocumentsProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	test.Cleanup(cancel)
	root, err := Open(ctx, "127.0.0.1:0", "", true)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(root.Close)
	for _, roleName := range []outbound.ProcessRole{outbound.ProcessMCP, outbound.ProcessWorker} {
		role, err := budget.ForRole(roleName)
		if err != nil {
			test.Fatal(err)
		}
		dataDSN, err := executionDataDSN(dsn, role)
		if err != nil {
			test.Fatal(err)
		}
		peer, err := store.NewPostgresStore(ctx, dataDSN)
		if err != nil {
			test.Fatal(err)
		}
		test.Cleanup(peer.Close)
		configuration, err := budget.AdmissionConfig(roleName)
		if err != nil {
			test.Fatal(err)
		}
		admission, err := peer.OpenProcessAdmission(ctx, configuration)
		if err != nil {
			test.Fatal("register only the owned local peer control session", err)
		}
		test.Cleanup(func() { _ = admission.Close() })
	}
	executionRootWait(test, func(context.Context) bool { return root.execution.ready() })
	capability := executionProfileSpoolCapability(test, root.Store)
	limit := root.execution.role.SpoolBytes
	entered, canceled, release, handled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	released := false
	unblock := func() {
		if !released {
			close(release)
			released = true
		}
	}
	defer unblock()
	handler := root.execution.handler(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(entered)
		<-request.Context().Done()
		close(canceled)
		<-release
	}))
	go func() {
		defer close(handled)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/spool-drain", nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		test.Fatal("actual profile handler did not admit the owned request")
	}
	closed := make(chan error, 1)
	joined := false
	go func() {
		root.Close()
		closed <- root.infrastructure.Close()
	}()
	defer func() {
		unblock()
		if !joined {
			select {
			case err := <-closed:
				if err != nil {
					test.Error(err)
				}
			case <-time.After(5 * time.Second):
				test.Error("root close did not join after releasing the owned request")
			}
		}
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		test.Fatal("profile close did not cancel the active request")
	}
	select {
	case err := <-closed:
		joined = true
		test.Fatal("profile close returned before the request drained", err)
	default:
	}
	executionProfileSpoolUsage(test, capability, limit)
	if overlap, err := capability.ConfigureTransientSpoolBudget(limit); err == nil || overlap != nil {
		if overlap != nil {
			_ = overlap()
		}
		test.Fatal("active request drainage prematurely released the spool configuration", err)
	}
	unblock()
	select {
	case err := <-closed:
		joined = true
		if err != nil {
			test.Fatal("request-drained root cleanup failed", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("profile close did not finish after request drainage")
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		test.Fatal("owned request handler did not return after root cleanup")
	}
	executionProfileSpoolUsage(test, capability, 0)
}
