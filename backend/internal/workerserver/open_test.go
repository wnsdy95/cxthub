package workerserver

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

func wave48ProviderRootEnvironment(test *testing.T) string {
	test.Helper()
	wave48ClearWorkerSources(test)
	test.Setenv("CXT_PUBLIC_URL", "https://example.test")
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_AUTH", "invalid-must-not-be-constructed")
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedWorkersProfile)
	budget := executionprofile.Budget{
		Version: 1, Profile: outbound.BoundedWorkersProfile,
		BaselineActive: 4, BaselineBulk: 2, BaselineConnections: 20, ReservedConnections: 7,
		BaselineCPU: 4, BaselineMemoryBytes: 800 << 20, BaselineSpoolBytes: 3 << 30,
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		entry := executionprofile.RoleBudget{
			Role: role, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2,
			RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 1 << 30,
		}
		if role == outbound.ProcessAPI {
			entry.Active, entry.DataPoolMax = 2, 3
		}
		budget.Roles = append(budget.Roles, entry)
	}
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	path := filepath.Join(test.TempDir(), "budget.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		test.Fatal(err)
	}
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", path)
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	return data
}

func TestWave48ProviderRootValidatesInputsBeforeStorage(test *testing.T) {
	for _, variable := range []string{"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS", "CXT_MEMORY_PREPARATION_REPOS", "CXT_GITHUB_APP_CLIENT_SECRET"} {
		test.Run(variable, func(test *testing.T) {
			data := wave48ProviderRootEnvironment(test)
			test.Setenv(variable, "wave48-secret-input")
			worker, err := OpenWorker(context.Background())
			if worker != nil || err == nil || strings.Contains(err.Error(), "wave48-secret-input") || strings.Contains(err.Error(), "CXT_POSTGRES_DSN") {
				test.Fatal("malformed provider input reached storage or exposed its value", err)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				test.Fatal("malformed provider input touched local storage", err)
			}
		})
	}
}

func TestWave48DeliveryWorkerDoesNotReinterpretStoredEmailSettings(test *testing.T) {
	data := wave48ProviderRootEnvironment(test)
	test.Setenv("RESEND_API_KEY", "wave48-secret-resend")
	test.Setenv("RESEND_FROM", "unused-invalid-rendering-sender")
	test.Setenv("CXT_WEB_URL", "https://user:unused-rendering-secret@example.test")
	worker, err := OpenWorker(context.Background())
	if worker != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN") || strings.Contains(err.Error(), "secret") {
		test.Fatal("delivery worker required API rendering settings or bypassed PostgreSQL validation", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("delivery worker fell back to local storage", err)
	}
}
