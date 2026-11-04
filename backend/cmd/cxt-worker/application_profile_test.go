package main

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

const wave48ApplicationProfile = "bounded-workers-v1"

func wave48CommandEnvironment(test *testing.T) string {
	test.Helper()
	test.Setenv("CXT_ENV_FILE", os.DevNull)
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_AUTH", "invalid")
	test.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	for _, variable := range []string{
		"CXT_MEMORY_READ_REPOS", "CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS",
		"CXT_GRAPH_SHADOW_REPOS", "CXT_MEMORY_PREPARATION_REPOS", "CXT_GRAPH_READ_REPOS",
		"CXT_GITHUB_TOKEN", "CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID",
		"CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG",
		"CXT_GITHUB_APP_WEBHOOK_SECRET", "RESEND_API_KEY", "RESEND_FROM", "CXT_WEB_URL",
		"CXT_LEGACY_BYTE_RANGE_PUBLICATION", "CXT_LEGACY_DERIVED_BYTE_PUBLICATION",
	} {
		test.Setenv(variable, "")
	}
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	return data
}

func wave48CommandBudgetFile(test *testing.T, profile string) string {
	test.Helper()
	budget := executionprofile.Budget{
		Version: 1, Profile: profile,
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
	return path
}

func wave48AssertCommandError(test *testing.T, err error, data, want string, category error) {
	test.Helper()
	if err == nil || category != nil && !errors.Is(err, category) || want != "" && !strings.Contains(err.Error(), want) {
		test.Fatalf("worker argv did not reject configuration at %q", want)
	}
	if strings.Contains(err.Error(), "wave48-secret") {
		test.Fatal("worker argv exposed a configuration secret")
	}
	if _, statErr := os.Stat(data); !errors.Is(statErr, os.ErrNotExist) {
		test.Fatal("rejected worker argv touched local data storage")
	}
}

func TestWave48WorkerArgvRequiresExplicitProfileAndManifest(test *testing.T) {
	for _, profile := range []string{"", "embedded", "future-profile", outbound.BoundedDocumentsProfile, wave48ApplicationProfile} {
		test.Run("profile="+profile, func(test *testing.T) {
			data := wave48CommandEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", profile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
			test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
			want, category := "CXT_EXECUTION_PROFILE", domain.ErrValidation
			if profile == "future-profile" {
				category = errors.ErrUnsupported
			}
			if profile == outbound.BoundedDocumentsProfile || profile == wave48ApplicationProfile {
				want = "CXT_EXECUTION_BUDGET_FILE"
			}
			wave48AssertCommandError(test, run(context.Background(), []string{"run"}), data, want, category)
		})
	}
}

func TestWave48WorkerArgvRejectsProfileManifestMismatchBeforeStorage(test *testing.T) {
	for _, selection := range []struct {
		environment string
		manifest    string
	}{
		{environment: wave48ApplicationProfile, manifest: outbound.BoundedDocumentsProfile},
		{environment: outbound.BoundedDocumentsProfile, manifest: wave48ApplicationProfile},
	} {
		test.Run(selection.environment, func(test *testing.T) {
			data := wave48CommandEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", selection.environment)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", wave48CommandBudgetFile(test, selection.manifest))
			test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
			wave48AssertCommandError(test, run(context.Background(), []string{"run"}), data, "profile", domain.ErrValidation)
		})
	}
}

func TestWave48WorkerArgvBothProfilesRequirePostgres(test *testing.T) {
	for _, profile := range []string{outbound.BoundedDocumentsProfile, wave48ApplicationProfile} {
		test.Run(profile, func(test *testing.T) {
			data := wave48CommandEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", profile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", wave48CommandBudgetFile(test, profile))
			wave48AssertCommandError(test, run(context.Background(), []string{"run"}), data, "CXT_POSTGRES_DSN", domain.ErrValidation)
		})
	}
}

func TestWave48WorkerArgvValidatesApplicationSelectionsAndProvidersBeforePostgres(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		variable string
		value    string
		want     string
	}{
		{name: "pending projection", variable: "CXT_PENDING_PROJECTION_REPOS", value: "wave48-secret-not-a-hash", want: "CXT_PENDING_PROJECTION_REPOS"},
		{name: "graph projection", variable: "CXT_GRAPH_PROJECTION_REPOS", value: "wave48-secret-not-a-hash", want: "CXT_GRAPH_PROJECTION_REPOS"},
		{name: "graph qualification", variable: "CXT_GRAPH_SHADOW_REPOS", value: "wave48-secret-not-a-hash", want: "CXT_GRAPH_SHADOW_REPOS"},
		{name: "memory preparation", variable: "CXT_MEMORY_PREPARATION_REPOS", value: "wave48-secret-not-a-hash", want: "CXT_MEMORY_PREPARATION_REPOS"},
		{name: "partial GitHub", variable: "CXT_GITHUB_APP_CLIENT_SECRET", value: "wave48-secret-client", want: "GitHub"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			data := wave48CommandEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", wave48ApplicationProfile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", wave48CommandBudgetFile(test, wave48ApplicationProfile))
			test.Setenv(scenario.variable, scenario.value)
			wave48AssertCommandError(test, run(context.Background(), []string{"run"}), data, scenario.want, nil)
		})
	}
}

func TestWave48WorkerArgvSanitizesInvalidPostgresConfiguration(test *testing.T) {
	data := wave48CommandEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", wave48ApplicationProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", wave48CommandBudgetFile(test, wave48ApplicationProfile))
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
	wave48AssertCommandError(test, run(context.Background(), []string{"run"}), data, "PostgreSQL", domain.ErrValidation)
}
