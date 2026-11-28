package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestPreciseCommandFailureEnvelope(t *testing.T) {
	for _, tc := range []struct {
		err         error
		code, phase string
		exit        int
		retry       bool
	}{
		{fmt.Errorf("invalid target: %w", domain.ErrInvalidRef), "invalid_ref", "command", 2, false},
		{fmt.Errorf("upload: %w", context.Canceled), "cancelled", "command", 130, false},
		{fmt.Errorf("upload: %w", domain.ErrInvalidCIR), "source_corrupt", "command", 5, false},
		{fmt.Errorf("read: %w", domain.ErrUnsupportedCIRVersion), "unsupported_version", "command", 5, false},
		{domain.ErrSyncConflict, "conflict", "command", 3, false},
		{domain.ErrSelectionChanged, "position_changed", "command", 3, true},
		{errors.New("position_changed source_corrupt invalid ref sync conflict"), "needs_attention", "command", 1, false},
	} {
		var output bytes.Buffer
		exit := WriteCommandFailure(&output, []string{"cxt", "push", "--json"}, tc.err)
		var envelope struct {
			Error CommandFailure `json:"error"`
		}
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		got := envelope.Error
		if exit != tc.exit || got.Code != tc.code || got.Phase != tc.phase || got.Retryable != tc.retry || got.State != "inspect_receipts" {
			t.Fatalf("%v: %+v", tc.err, got)
		}
	}
}
