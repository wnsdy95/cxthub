package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestAPIActualServeArgvRejectsProfileBeforeStore(test *testing.T) {
	test.Setenv("CXT_ENV_FILE", os.DevNull)
	test.Setenv("CXT_AUTH", "dev")
	test.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	test.Setenv("CXT_MEMORY_READ_REPOS", "")
	test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:credential-secret@localhost/untouched")
	test.Setenv("CXT_EXECUTION_PROFILE", "unknown")
	data := filepath.Join(test.TempDir(), "untouched")
	err := serve(context.Background(), []string{"--addr", "127.0.0.1:0", "--data", data})
	if !errors.Is(err, errors.ErrUnsupported) || strings.Contains(err.Error(), "credential-secret") {
		test.Fatal("actual API serve argv bypassed profile validation")
	}
	test.Setenv("CXT_EXECUTION_PROFILE", "bounded-documents-v1")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	err = serve(context.Background(), []string{"--addr", "127.0.0.1:0", "--data", data})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_EXECUTION_BUDGET_FILE") {
		test.Fatal("actual API serve argv reached storage before its explicit budget", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("invalid profile touched API storage", err)
	}
}
