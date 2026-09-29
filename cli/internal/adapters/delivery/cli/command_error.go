package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type argumentError struct{ error }

// CommandFailure is the versioned automation boundary. It does not infer a
// completed/rolled-back mutation from a human error message. Durable operations
// expose their exact state and ID through status/capture/commit receipts.
type CommandFailure struct {
	Version   int    `json:"version"`
	Code      string `json:"code"`
	Phase     string `json:"phase"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	ExitCode  int    `json:"exit_code"`
	State     string `json:"mutation_state"`
}

func ClassifyCommandFailure(err error) CommandFailure {
	out := CommandFailure{Version: 1, Code: "needs_attention", Phase: "command", Message: err.Error(), ExitCode: 1, State: "inspect_receipts"}
	var args argumentError
	var status interface{ StatusCode() int }
	switch {
	case errors.As(err, &args):
		out.Code = "invalid_arguments"
		out.Phase = "preflight"
		out.ExitCode = 2
		out.State = "unchanged"
	case errors.Is(err, context.Canceled):
		out.Code = "cancelled"
		out.ExitCode = 130
	case errors.Is(err, context.DeadlineExceeded):
		out.Code = "timeout"
		out.Retryable = true
	case errors.Is(err, domain.ErrIndexChanged):
		out.Code = "index_changed"
		out.ExitCode = 3
	case errors.Is(err, domain.ErrCodePositionMismatch):
		out.Code = "code_position_mismatch"
		out.ExitCode = 3
		out.Retryable = true
	case errors.Is(err, domain.ErrSelectionChanged):
		out.Code = "position_changed"
		out.ExitCode = 3
		out.Retryable = true
	case errors.Is(err, domain.ErrSyncConflict), errors.Is(err, domain.ErrMemoryContention):
		out.Code = "conflict"
		out.ExitCode = 3
	case errors.Is(err, domain.ErrHashMismatch), errors.Is(err, domain.ErrInvalidCIR):
		out.Code = "source_corrupt"
		out.ExitCode = 5
	case errors.Is(err, domain.ErrProviderCapabilityUnknown):
		out.Code = "provider_capability_unknown"
		out.Phase = "prepare"
		out.ExitCode = 6
	case errors.Is(err, domain.ErrContextBudgetExceeded):
		out.Code = "context_budget_exceeded"
		out.Phase = "prepare"
		out.ExitCode = 6
	case errors.Is(err, domain.ErrStagingVersion), errors.Is(err, domain.ErrUnsupportedCIRVersion):
		out.Code = "unsupported_version"
		out.ExitCode = 5
	case errors.Is(err, domain.ErrEmptyIndex):
		out.Code = "empty_index"
		out.ExitCode = 3
	case errors.Is(err, domain.ErrInvalidRef):
		out.Code = "invalid_ref"
		out.ExitCode = 2
	case errors.Is(err, domain.ErrNotFound):
		out.Code = "not_found"
		out.ExitCode = 1
	case errors.Is(err, domain.ErrDeliveryFailed):
		out.Code = "delivery_failed"
		out.Phase = "delivery"
	case errors.As(err, &status):
		switch status.StatusCode() {
		case 401, 403:
			out.Code = "permission_denied"
			out.ExitCode = 4
		case 409, 412:
			out.Code = "conflict"
			out.ExitCode = 3
		case 429, 502, 503, 504:
			out.Code = "server_unavailable"
			out.Retryable = true
		}
	}
	return out
}

// WriteCommandFailure never consumes provider-owned --json flags. Public cxt
// commands that advertise --json receive a structured error on stderr.
func WriteCommandFailure(w io.Writer, args []string, err error) int {
	failure := ClassifyCommandFailure(err)
	machine := false
	if len(args) > 1 {
		if spec, ok := commandArgSpecs[args[1]]; ok && !spec.passthrough {
			if _, ok = spec.flags["--json"]; ok {
				for _, arg := range args[2:] {
					if arg == "--json" {
						machine = true
					}
				}
			}
		}
	}
	if machine {
		_ = json.NewEncoder(w).Encode(struct {
			Error CommandFailure `json:"error"`
		}{failure})
	} else {
		_, _ = fmt.Fprintf(w, "cxt: %v\n", err)
	}
	return failure.ExitCode
}
