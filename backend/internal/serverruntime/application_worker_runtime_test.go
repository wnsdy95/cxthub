package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const wave48ApplicationWorkerProfile = "bounded-workers-v1"

func wave48WorkerEnvironment(test *testing.T) string {
	test.Helper()
	infrastructureRuntimeEnvironment(test)
	for _, variable := range []string{
		"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS",
		"CXT_MEMORY_PREPARATION_REPOS", "CXT_GRAPH_READ_REPOS", "CXT_GITHUB_TOKEN",
		"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET",
		"CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET",
		"RESEND_API_KEY", "RESEND_FROM", "CXT_WEB_URL",
	} {
		test.Setenv(variable, "")
	}
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	return data
}

func wave48WorkerBudget(test *testing.T, profile string) []byte {
	test.Helper()
	var budget executionprofile.Budget
	if err := json.Unmarshal(runtimeExecutionBudget(test), &budget); err != nil {
		test.Fatal(err)
	}
	budget.Profile = profile
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func wave48WorkerStartupError(test *testing.T, worker *WorkerRuntime, err error, data, want string, category error) {
	test.Helper()
	if worker != nil {
		test.Cleanup(func() { _ = worker.Close() })
		test.Fatal("invalid worker configuration opened infrastructure")
	}
	if err == nil || category != nil && !errors.Is(err, category) || want != "" && !strings.Contains(err.Error(), want) {
		test.Fatalf("worker startup did not reject configuration at %q", want)
	}
	if strings.Contains(err.Error(), "wave48-secret") {
		test.Fatal("worker startup exposed a configuration secret")
	}
	if _, statErr := os.Stat(data); !errors.Is(statErr, os.ErrNotExist) {
		test.Fatal("rejected worker startup touched local data storage")
	}
}

func TestWave48WorkerRootsRequireExplicitProfileAndManifest(test *testing.T) {
	for name, open := range map[string]func(context.Context) (*WorkerRuntime, error){
		"worker": func(ctx context.Context) (*WorkerRuntime, error) { return OpenWorker(ctx) }, "documents": OpenDocumentWorker,
	} {
		test.Run(name, func(test *testing.T) {
			for _, profile := range []string{"", "embedded", "future-profile", outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
				if name == "documents" && profile == wave48ApplicationWorkerProfile {
					continue
				}
				test.Run("profile="+profile, func(test *testing.T) {
					data := wave48WorkerEnvironment(test)
					test.Setenv("CXT_EXECUTION_PROFILE", profile)
					test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
					test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
					want, category := "CXT_EXECUTION_PROFILE", domain.ErrValidation
					if profile == "future-profile" {
						category = errors.ErrUnsupported
					}
					if profile == outbound.BoundedDocumentsProfile || profile == wave48ApplicationWorkerProfile {
						want = "CXT_EXECUTION_BUDGET_FILE"
					}
					worker, err := open(context.Background())
					wave48WorkerStartupError(test, worker, err, data, want, category)
				})
			}
		})
	}
}

func TestWave48WorkerRejectsProfileManifestMismatchBeforeStorage(test *testing.T) {
	for _, selection := range []struct {
		environment string
		manifest    string
	}{
		{environment: wave48ApplicationWorkerProfile, manifest: outbound.BoundedDocumentsProfile},
		{environment: outbound.BoundedDocumentsProfile, manifest: wave48ApplicationWorkerProfile},
	} {
		test.Run(selection.environment, func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", selection.environment)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, selection.manifest)))
			test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
			worker, err := OpenWorker(context.Background())
			wave48WorkerStartupError(test, worker, err, data, "profile", domain.ErrValidation)
		})
	}
}

func TestWave48DocumentWorkerRejectsApplicationProfileBeforeStorage(test *testing.T) {
	data := wave48WorkerEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", wave48ApplicationWorkerProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, wave48ApplicationWorkerProfile)))
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
	worker, err := OpenDocumentWorker(context.Background())
	wave48WorkerStartupError(test, worker, err, data, "", domain.ErrValidation)
	if !strings.Contains(err.Error(), outbound.BoundedDocumentsProfile) && !strings.Contains(err.Error(), wave48ApplicationWorkerProfile) && !strings.Contains(err.Error(), "OpenWorker") {
		test.Fatal("document-only root reached PostgreSQL validation instead of rejecting the full profile")
	}
}

func TestWave48WorkerProfilesRequirePostgresWithoutAuthenticationDependencies(test *testing.T) {
	for _, profile := range []string{outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
		test.Run(profile, func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			test.Setenv("CXT_AUTH", "invalid")
			test.Setenv("CXT_MEMORY_READ_REPOS", "invalid")
			test.Setenv("CXT_EXECUTION_PROFILE", profile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, profile)))
			worker, err := OpenWorker(context.Background())
			wave48WorkerStartupError(test, worker, err, data, "CXT_POSTGRES_DSN", domain.ErrValidation)
		})
	}
}

func TestWave48RuntimeExternalWorkersIsFullProfileOnly(test *testing.T) {
	for _, profile := range []string{"", outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
		test.Run("profile="+profile, func(test *testing.T) {
			root := &Runtime{}
			if profile != "" {
				root.execution = &executionProfile{budget: executionprofile.Budget{Profile: profile}}
			}
			if root.ExternalWorkers() != (profile == wave48ApplicationWorkerProfile) {
				test.Fatal("external application ownership changed an existing execution profile")
			}
			if root.BoundedDocuments() != (profile != "") {
				test.Fatal("application ownership changed bounded document ownership")
			}
		})
	}
}
