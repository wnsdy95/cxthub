package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPNeverFallsBackToFilesystem(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CXT_ENV_FILE", os.DevNull)
	t.Setenv("CXT_POSTGRES_DSN", "")
	t.Setenv("CXT_AUTH", "dev")
	t.Setenv("CXT_PUBLIC_URL", "http://localhost:5173")
	t.Setenv("CXT_DATA", filepath.Join(dir, "must-not-create"))
	err := serve(context.Background(), []string{"--addr", "127.0.0.1:8908"})
	if err == nil || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN") {
		t.Fatalf("missing DSN: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("MCP touched the filesystem: %v, %v", entries, err)
	}
}

func TestInvalidMCPPublicOriginFailsBeforeDatabaseConnection(t *testing.T) {
	t.Setenv("CXT_ENV_FILE", os.DevNull)
	t.Setenv("CXT_POSTGRES_DSN", "invalid-database-sentinel")
	t.Setenv("CXT_PUBLIC_URL", "https://cxthub.io")
	for _, origin := range []string{"http://mcp.cxthub.io", "https://mcp.cxthub.io/mcp", "https://user:secret@mcp.cxthub.io"} {
		t.Setenv("CXT_MCP_PUBLIC_URL", origin)
		err := serve(context.Background(), []string{"--addr", ":8908"})
		if err == nil || !strings.Contains(err.Error(), "CXT_MCP_PUBLIC_URL") {
			t.Fatalf("invalid resource origin did not fail first: %v", err)
		}
	}
}
