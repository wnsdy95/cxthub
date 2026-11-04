package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func fullRuntimeProfileBudget(test *testing.T, profileName string) executionprofile.Budget {
	test.Helper()
	var budget executionprofile.Budget
	if err := json.Unmarshal(runtimeExecutionBudget(test), &budget); err != nil {
		test.Fatal(err)
	}
	budget.Profile = profileName
	return budget
}

func fullRuntimeProfileBudgetFile(test *testing.T, budget executionprofile.Budget) string {
	test.Helper()
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	return runtimeExecutionBudgetFile(test, raw)
}

func requireFullProfileNoStoreOrRuntimeEffects(test *testing.T, data string, previousCPU int, previousMemory int64) {
	test.Helper()
	if runtime.GOMAXPROCS(0) != previousCPU || debug.SetMemoryLimit(-1) != previousMemory {
		test.Fatal("execution profile selection changed process runtime settings")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("execution profile selection touched data storage", err)
	}
}

func TestFullExecutionProfileSelectsOnlyExplicitExternalWorkersWithoutStartingExecution(test *testing.T) {
	for _, profileName := range []string{outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		test.Run(profileName, func(test *testing.T) {
			budget := fullRuntimeProfileBudget(test, profileName)
			test.Setenv("CXT_EXECUTION_PROFILE", " \t"+profileName+"\n")
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, budget))
			test.Setenv("CXT_POSTGRES_DSN", "invalid-dsn-must-not-be-parsed")
			data := filepath.Join(test.TempDir(), "untouched")
			test.Setenv("CXT_DATA", data)
			previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			for _, expected := range budget.Roles {
				profile, err := executionProfileFromEnvironment(expected.Role, expected.Role == outbound.ProcessWorker)
				if err != nil || profile == nil {
					test.Fatal("explicit bounded profile was rejected", profile, err)
				}
				if profile.role != expected || !reflect.DeepEqual(profile.budget, budget) {
					test.Fatal("bounded profile changed the declared role or resource envelopes")
				}
				if profile.admission != nil || profile.ctx != nil || profile.cancel != nil || profile.restore != nil || profile.controlDone != nil {
					test.Fatal("profile selection started execution or acquired process admission")
				}
				selected := &Runtime{execution: profile}
				if !selected.BoundedDocuments() || selected.ExternalWorkers() != (profileName == outbound.BoundedWorkersProfile) {
					test.Fatal("external worker selection changed document ownership or enabled the wrong worker set")
				}
			}
			requireFullProfileNoStoreOrRuntimeEffects(test, data, previousCPU, previousMemory)
		})
	}
}

func TestFullExecutionProfileDefaultsDoNotReadManifestOrEnableExternalWorkers(test *testing.T) {
	manifest := fullRuntimeProfileBudgetFile(test, fullRuntimeProfileBudget(test, outbound.BoundedWorkersProfile))
	missing := filepath.Join(test.TempDir(), "missing-budget.json")
	for _, profileName := range []string{"", " \t\n", "embedded", " embedded "} {
		test.Run(profileName, func(test *testing.T) {
			test.Setenv("CXT_EXECUTION_PROFILE", profileName)
			test.Setenv("CXT_POSTGRES_DSN", "invalid-dsn-must-not-be-parsed")
			data := filepath.Join(test.TempDir(), "untouched")
			test.Setenv("CXT_DATA", data)
			previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			for _, path := range []string{manifest, missing} {
				test.Setenv("CXT_EXECUTION_BUDGET_FILE", path)
				for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP} {
					profile, err := executionProfileFromEnvironment(role, false)
					if err != nil || profile != nil {
						test.Fatal("embedded default consumed a full worker manifest", profile, err)
					}
					selected := &Runtime{execution: profile}
					if selected.BoundedDocuments() || selected.ExternalWorkers() {
						test.Fatal("embedded default enabled bounded document or full worker ownership")
					}
				}
				if profile, err := executionProfileFromEnvironment(outbound.ProcessWorker, true); profile != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("worker accepted implicit profile selection from the manifest", profile, err)
				}
			}
			requireFullProfileNoStoreOrRuntimeEffects(test, data, previousCPU, previousMemory)
		})
	}
}

func TestFullExecutionProfileRequiresExplicitSupportedNameAndManifest(test *testing.T) {
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", filepath.Join(test.TempDir(), "must-not-read"))
	for _, profileName := range []string{"bounded-workers-v2", "Bounded-workers-v1", "bounded-workers-v1 extra"} {
		test.Setenv("CXT_EXECUTION_PROFILE", profileName)
		if profile, err := executionProfileFromEnvironment(outbound.ProcessAPI, false); profile != nil || !errors.Is(err, errors.ErrUnsupported) {
			test.Fatal("unsupported full worker profile did not fail closed", profile, err)
		}
	}
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedWorkersProfile)
	for _, path := range []string{
		"",
		filepath.Join(test.TempDir(), "missing"),
		runtimeExecutionBudgetFile(test, []byte(`{"version":1}`)),
		runtimeExecutionBudgetFile(test, []byte(strings.Repeat(" ", executionBudgetLimit+1))),
	} {
		test.Setenv("CXT_EXECUTION_BUDGET_FILE", path)
		if profile, err := executionProfileFromEnvironment(outbound.ProcessWorker, true); profile != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("full worker profile accepted a missing or invalid manifest", profile, err)
		}
	}
}

func TestFullExecutionProfileRejectsStrictManifestMismatchBeforeStoreEffects(test *testing.T) {
	for _, profileName := range []string{outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		test.Run(profileName, func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			manifestProfile := outbound.BoundedDocumentsProfile
			if profileName == outbound.BoundedDocumentsProfile {
				manifestProfile = outbound.BoundedWorkersProfile
			}
			test.Setenv("CXT_EXECUTION_PROFILE", profileName)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, fullRuntimeProfileBudget(test, manifestProfile)))
			test.Setenv("CXT_POSTGRES_DSN", "invalid-dsn-must-not-be-parsed")
			data := filepath.Join(test.TempDir(), "untouched")
			test.Setenv("CXT_DATA", data)
			previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
				profile, err := executionProfileFromEnvironment(role, role == outbound.ProcessWorker)
				if profile != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "must match") {
					test.Fatal("environment and manifest profile mismatch was not rejected", profile, err)
				}
			}
			for rootName, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"api": Open, "mcp": OpenMCP} {
				test.Run(rootName, func(test *testing.T) {
					opened, err := open(context.Background(), "127.0.0.1:0", data, true)
					if opened != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "must match") {
						test.Fatal("profile mismatch reached store initialization", opened, err)
					}
				})
			}
			requireFullProfileNoStoreOrRuntimeEffects(test, data, previousCPU, previousMemory)
		})
	}
}

func TestFullExecutionProfileRejectsAggregateBudgetsBeforeStoreEffects(test *testing.T) {
	testCases := []struct {
		name   string
		detail string
		change func(*executionprofile.Budget)
	}{
		{"connections", "connection baseline", func(budget *executionprofile.Budget) { budget.BaselineConnections = 19 }},
		{"operator reserve", "connection baseline", func(budget *executionprofile.Budget) { budget.ReservedConnections = 8 }},
		{"CPU", "baseline budgets", func(budget *executionprofile.Budget) { budget.BaselineCPU = 2 }},
		{"memory", "baseline budgets", func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes = 575 << 20 }},
		{"spool", "baseline budgets", func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes-- }},
		{"queue", "queue limit", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = 9 }},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			budget := fullRuntimeProfileBudget(test, outbound.BoundedWorkersProfile)
			testCase.change(&budget)
			test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedWorkersProfile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, budget))
			test.Setenv("CXT_POSTGRES_DSN", "invalid-dsn-must-not-be-parsed")
			data := filepath.Join(test.TempDir(), "untouched")
			previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			for rootName, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"api": Open, "mcp": OpenMCP} {
				test.Run(rootName, func(test *testing.T) {
					opened, err := open(context.Background(), "127.0.0.1:0", data, false)
					if opened != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), testCase.detail) {
						test.Fatal("full worker aggregate rejection did not precede storage", opened, err)
					}
				})
			}
			requireFullProfileNoStoreOrRuntimeEffects(test, data, previousCPU, previousMemory)
		})
	}
}

func TestFullExecutionProfileRequiresPostgresWithoutFilesystemFallback(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedWorkersProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, fullRuntimeProfileBudget(test, outbound.BoundedWorkersProfile)))
	data := filepath.Join(test.TempDir(), "untouched")
	previousCPU, previousMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	for rootName, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"api": Open, "mcp": OpenMCP} {
		test.Run(rootName, func(test *testing.T) {
			opened, err := open(context.Background(), "127.0.0.1:0", data, false)
			if opened != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN") {
				test.Fatal("full worker profile permitted filesystem fallback", opened, err)
			}
		})
	}
	requireFullProfileNoStoreOrRuntimeEffects(test, data, previousCPU, previousMemory)
}

func TestFullExecutionProfileKeepsSharedPostgresDataLabels(test *testing.T) {
	budget := fullRuntimeProfileBudget(test, outbound.BoundedWorkersProfile)
	for _, role := range budget.Roles {
		test.Run(string(role.Role), func(test *testing.T) {
			for _, raw := range []string{
				"postgres://role:secret@localhost/database?application_name=cxthub/bounded-workers-v1/untrusted/data&pool_max_conns=99&pool_min_conns=4&pool_min_idle_conns=2",
				"host=localhost user=role password=secret dbname=database application_name=cxthub/bounded-workers-v1/untrusted/data pool_max_conns=99 pool_min_conns=4 pool_min_idle_conns=2",
			} {
				rewritten, err := executionDataDSN(raw, role)
				if err != nil {
					test.Fatal("full worker data DSN rewrite failed", err)
				}
				parsed, err := pgxpool.ParseConfig(rewritten)
				if err != nil {
					test.Fatal("full worker data DSN could not be parsed", err)
				}
				expected := "cxthub/" + outbound.BoundedDocumentsProfile + "/" + string(role.Role) + "/data"
				if parsed.MaxConns != role.DataPoolMax || parsed.MinConns != 0 || parsed.MinIdleConns != 0 || parsed.ConnConfig.RuntimeParams["application_name"] != expected {
					test.Fatal("full worker profile escaped the shared PostgreSQL admission data family")
				}
			}
		})
	}
}
