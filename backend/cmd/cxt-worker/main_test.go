package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestWorkerActualArgvRequiresRun(test *testing.T) {
	test.Setenv("CXT_ENV_FILE", filepath.Join(test.TempDir(), "missing"))
	for _, args := range [][]string{nil, {"serve"}, {"run", "--data", "local"}, {"run", "unexpected"}} {
		if err := run(context.Background(), args); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "usage: cxt-worker run") {
			test.Fatal("worker argv reached environment or infrastructure before usage validation", err)
		}
	}
}

func TestWorkerActualArgvDoesNotImplicitlyRunEmbedded(test *testing.T) {
	test.Setenv("CXT_ENV_FILE", os.DevNull)
	test.Setenv("CXT_EXECUTION_PROFILE", "")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	test.Setenv("CXT_POSTGRES_DSN", "")
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	if err := run(context.Background(), []string{"run"}); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_EXECUTION_PROFILE") {
		test.Fatal("worker actual argv bypassed bounded composition root", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("worker argv created local data", err)
	}
}

func TestWorkerActualArgvRequiresManifest(test *testing.T) {
	test.Setenv("CXT_ENV_FILE", os.DevNull)
	test.Setenv("CXT_EXECUTION_PROFILE", "bounded-documents-v1")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:credential-secret@localhost/no-connect")
	err := run(context.Background(), []string{"run"})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_EXECUTION_BUDGET_FILE") || strings.Contains(err.Error(), "credential-secret") {
		test.Fatal("worker actual argv connected or exposed DSN before manifest validation")
	}
}

func TestWorkerMainArgvChild(test *testing.T) {
	switch os.Getenv("CXT_WORKER_MAIN_CASE") {
	case "missing-profile":
		os.Args = []string{"cxt-worker", "run"}
	case "missing-manifest":
		os.Args = []string{"cxt-worker", "run"}
	case "wrong-command":
		os.Args = []string{"cxt-worker", "serve"}
	default:
		return
	}
	main()
	test.Fatal("invalid worker main argv returned successfully")
}

func TestWorkerMainActualArgvRejectsImplicitLocalRun(test *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	for name, want := range map[string]string{
		"missing-profile":  "CXT_EXECUTION_PROFILE",
		"missing-manifest": "CXT_EXECUTION_BUDGET_FILE",
		"wrong-command":    "usage: cxt-worker run",
	} {
		test.Run(name, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestWorkerMainArgvChild$")
			command.Dir = test.TempDir()
			profile := ""
			if name == "missing-manifest" {
				profile = "bounded-documents-v1"
			}
			command.Env = []string{
				"HOME=" + command.Dir, "CXT_ENV_FILE=" + os.DevNull,
				"CXT_WORKER_MAIN_CASE=" + name, "CXT_EXECUTION_PROFILE=" + profile,
				"CXT_EXECUTION_BUDGET_FILE=", "CXT_POSTGRES_DSN=postgres://user:credential-secret@localhost/untouched",
			}
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil {
				test.Fatal("worker main did not reject invalid actual argv promptly")
			}
			if !strings.Contains(string(output), want) || strings.Contains(string(output), "credential-secret") {
				test.Fatal("worker main did not validate before opening storage, or exposed raw DSN")
			}
		})
	}
}
