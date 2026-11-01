package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMCPActualServeArgvRejectsProfileBeforeStore(test *testing.T) {
	test.Setenv("CXT_ENV_FILE", os.DevNull)
	test.Setenv("CXT_AUTH", "dev")
	test.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	test.Setenv("CXT_MCP_PUBLIC_URL", "https://mcp.cxthub.example")
	test.Setenv("CXT_MEMORY_READ_REPOS", "")
	test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
	test.Setenv("CXT_READ_PREPARATION_API_ORIGIN", "")
	test.Setenv("CXT_MEMORY_PREPARATION_API_ORIGIN", "")
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:credential-secret@localhost/untouched")
	test.Setenv("CXT_EXECUTION_PROFILE", "unknown")
	err := serve(context.Background(), []string{"--addr", "127.0.0.1:0"})
	if !errors.Is(err, errors.ErrUnsupported) || strings.Contains(err.Error(), "credential-secret") {
		test.Fatal("actual MCP serve argv bypassed profile validation")
	}
	test.Setenv("CXT_EXECUTION_PROFILE", "bounded-documents-v1")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	err = serve(context.Background(), []string{"--addr", "127.0.0.1:0"})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_EXECUTION_BUDGET_FILE") {
		test.Fatal("actual MCP serve argv reached storage before its explicit budget", err)
	}
}
