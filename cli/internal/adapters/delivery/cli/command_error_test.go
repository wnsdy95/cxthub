package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"strings"
	"testing"
)

func TestMachineCommandFailureIsStableAndProviderFlagsStayNative(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
		exit int
	}{
		{fmt.Errorf("wrapped: %w", domain.ErrSelectionChanged), "position_changed", 3},
		{domain.ErrHashMismatch, "source_corrupt", 5},
		{domain.ErrProviderCapabilityUnknown, "provider_capability_unknown", 6},
		{context.Canceled, "cancelled", 130},
	} {
		var b bytes.Buffer
		if exit := WriteCommandFailure(&b, []string{"cxt", "status", "--json"}, tc.err); exit != tc.exit {
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
	_, err := PreflightArgs([]string{"cxt", "add", "--expect", "not-a-hash", "--json"})
	if err == nil {
		t.Fatal("bad revision passed preflight")
	}
	failure := ClassifyCommandFailure(err)
	if failure.Code != "invalid_arguments" || failure.State != "unchanged" || failure.ExitCode != 2 {
		t.Fatalf("preflight: %+v", failure)
	}
	var b bytes.Buffer
	WriteCommandFailure(&b, []string{"cxt", "codex", "exec", "--json"}, domain.ErrNotFound)
	if !strings.HasPrefix(b.String(), "cxt: ") {
		t.Fatal("consumed native provider flag")
	}
}
