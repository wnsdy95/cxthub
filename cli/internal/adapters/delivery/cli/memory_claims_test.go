package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadExplicitMemoryClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	for name, raw := range map[string]string{
		"valid":   `[{"kind":"rationale","text":"Keep accepted history."}]`,
		"unknown": `[{"kind":"rationale","text":"Explicit.","invented":true}]`,
		"empty":   `[]`, "null": `null`, "trailing": `[] {}`, "oversized": strings.Repeat(" ", 4<<20+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readMemoryClaims(path)
			if name == "valid" {
				if err != nil || len(got) != 1 {
					t.Fatalf("claims=%+v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("bad claims accepted")
			}
		})
	}
	args := []string{"cxt", "memory", "--claims", path, "main"}
	if _, err := PreflightArgs(args); err != nil {
		t.Fatal(err)
	}
	if got := firstPositional(args[2:]); got != "main" {
		t.Fatalf("flag value consumed as ref: %s", got)
	}
}
