package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"strings"
	"testing"
)

func TestMachineCommandFailureIsStable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
		exit int
	}{
		{fmt.Errorf("wrapped: %w", domain.ErrSelectionChanged), "position_changed", 3},
		{domain.ErrHashMismatch, "source_corrupt", 5},
		{domain.ErrUnsupportedCIRVersion, "unsupported_version", 5},
		{context.Canceled, "cancelled", 130},
	} {
		var b bytes.Buffer
		if exit := WriteCommandFailure(&b, []string{"cxt", "doctor", "--json"}, tc.err); exit != tc.exit {
			t.Fatalf("exit=%d", exit)
		}
		var out struct {
			Error CommandFailure `json:"error"`
		}
		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error.Code != tc.code || out.Error.Version != 1 || out.Error.State == "unchanged" {
			t.Fatalf("bad failure: %+v", out)
		}
	}
	_, err := PreflightArgs([]string{"cxt", "push", "claude", "--json"})
	if err == nil {
		t.Fatal("a session named without its identifier passed preflight")
	}
	failure := ClassifyCommandFailure(err)
	if failure.Code != "invalid_arguments" || failure.State != "unchanged" || failure.ExitCode != 2 {
		t.Fatalf("preflight: %+v", failure)
	}
	// Without --json, and for a command that has no --json, the failure is plain text.
	for _, args := range [][]string{{"cxt", "doctor"}, {"cxt", "setup", "--json"}, {"cxt", "refresh"}} {
		var b bytes.Buffer
		WriteCommandFailure(&b, args, domain.ErrNotFound)
		if !strings.HasPrefix(b.String(), "cxt: ") {
			t.Fatalf("%v: %q", args, b.String())
		}
	}
}
