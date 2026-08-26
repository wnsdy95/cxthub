package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpAndUsageErrorsBeforeComposition(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))

	for _, args := range [][]string{
		{"cxt", "load", "--help"},
		{"cxt", "save", "-h"},
		{"cxt", "mcp", "--help"},
		{"cxt", "hook", "--help"},
		{"cxt", "version", "--help"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}

	for _, args := range [][]string{
		{"cxt", "load", "--unknown"},
		{"cxt", "save", "--unknown"},
		{"cxt", "mcp", "--unknown"},
		{"cxt", "hook", "--unknown"},
		{"cxt", "version", "--unknown"},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("run(%v) error = %v", args, err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("help/usage preflight mutated cwd: %+v", entries)
	}
}
