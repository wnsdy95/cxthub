package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestPreciseCommandFailureEnvelope(t *testing.T) {
	for _, tc := range []struct {
		err         error
		code, phase string
		exit        int
		retry       bool
	}{
		{fmt.Errorf("stale: %w", domain.ErrIndexChanged), "index_changed", "command", 3, false},
		{fmt.Errorf("moved: %w", domain.ErrCodePositionMismatch), "code_position_mismatch", "command", 3, true},
		{fmt.Errorf("invalid target: %w", domain.ErrInvalidRef), "invalid_ref", "command", 2, false},
		{fmt.Errorf("materialize: %w", domain.ErrDeliveryFailed), "delivery_failed", "delivery", 1, false},
		{fmt.Errorf("%w: %w", domain.ErrDeliveryFailed, context.Canceled), "cancelled", "command", 130, false},
		{fmt.Errorf("%w: %w", domain.ErrDeliveryFailed, domain.ErrInvalidCIR), "source_corrupt", "command", 5, false},
		{domain.ErrSyncConflict, "conflict", "command", 3, false},
		{domain.ErrSelectionChanged, "position_changed", "command", 3, true},
		{errors.New("delivery_failed index_changed code_position_mismatch invalid ref sync conflict"), "needs_attention", "command", 1, false},
	} {
		var output bytes.Buffer
		exit := WriteCommandFailure(&output, []string{"cxt", "commit", "--json"}, tc.err)
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

type typedConflictSync struct {
	inbound.SyncRepo
	err       error
	conflicts []string
}

func (f typedConflictSync) Push(context.Context, inbound.SyncInput) (inbound.SyncOutput, error) {
	return inbound.SyncOutput{}, f.err
}
func (f typedConflictSync) Pull(context.Context, inbound.SyncInput) (inbound.SyncOutput, error) {
	return inbound.SyncOutput{Conflicts: f.conflicts}, f.err
}

func TestSyncPresentationPreservesTypedConflictOnly(t *testing.T) {
	for _, tc := range []struct {
		command string
		sync    typedConflictSync
		want    string
	}{
		{"push", typedConflictSync{err: fmt.Errorf("ref: %w", domain.ErrSyncConflict)}, "conflict"},
		{"push", typedConflictSync{err: fmt.Errorf("memory attachment: %w", domain.ErrSyncConflict)}, "conflict"},
		{"push", typedConflictSync{err: errors.New("generic sync conflict text")}, "needs_attention"},
		{"pull", typedConflictSync{conflicts: []string{"main"}}, "conflict"},
	} {
		c := &Container{ResolveSyncDestination: func(context.Context, string, string) (SyncDestination, error) {
			return SyncDestination{Sync: tc.sync}, nil
		}}
		err := Run(c, []string{"cxt", tc.command, "origin", "main"})
		if err == nil || ClassifyCommandFailure(err).Code != tc.want {
			t.Fatalf("%s: %v", tc.command, err)
		}
	}
}

func TestPreparedDeliveryErrorsKeepCapabilityAndDeliverySeparate(t *testing.T) {
	request := ProviderLaunchRequest{Intent: LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000}}
	for _, tc := range []struct {
		name   string
		mutate func(*PreparedProviderLaunch)
		want   error
	}{
		{"invalid session", func(p *PreparedProviderLaunch) { p.SessionID = "invalid" }, domain.ErrDeliveryFailed},
		{"unverified host", func(p *PreparedProviderLaunch) { p.Capability = "unknown" }, domain.ErrProviderCapabilityUnknown},
		{"inexact counter", func(p *PreparedProviderLaunch) { p.TokenMeasurement = "estimated" }, domain.ErrProviderCapabilityUnknown},
		{"over budget", func(p *PreparedProviderLaunch) { p.SelectedTokens = 200001 }, domain.ErrContextBudgetExceeded},
	} {
		p := preparedLaunch(request)
		tc.mutate(&p)
		if err := validatePreparedProviderLaunch(request, p); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	cause := errors.New("receipt disk full")
	hooks := ProviderLaunchHooks{Prepare: func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		return preparedLaunch(request), nil
	}, Record: func(context.Context, ProviderLaunchReceipt) error { return cause }}
	_, _, err := prepareProviderLaunch(context.Background(), request, hooks)
	if !errors.Is(err, domain.ErrDeliveryFailed) || !errors.Is(err, cause) {
		t.Fatalf("receipt error lost: %v", err)
	}
	hooks.Prepare = func(context.Context, ProviderLaunchRequest) (PreparedProviderLaunch, error) {
		return PreparedProviderLaunch{}, errors.New("source unavailable")
	}
	hooks.Record = func(context.Context, ProviderLaunchReceipt) error { return nil }
	_, _, err = prepareProviderLaunch(context.Background(), request, hooks)
	if errors.Is(err, domain.ErrDeliveryFailed) {
		t.Fatal("generic preparation error misclassified as delivery")
	}
}

func TestPreparedDeliveryProcessFailureClassification(t *testing.T) {
	for _, failStart := range []bool{true, false} {
		name := "task exit"
		if failStart {
			name = "start"
		}
		t.Run(name, func(t *testing.T) {
			root, bin := providerLaunchFixture(t, "claude", "exit 7")
			hooks := ProviderLaunchHooks{Prepare: func(_ context.Context, req ProviderLaunchRequest) (PreparedProviderLaunch, error) {
				if failStart {
					if err := os.Remove(bin); err != nil {
						return PreparedProviderLaunch{}, err
					}
				}
				return preparedLaunch(req), nil
			}, Record: func(context.Context, ProviderLaunchReceipt) error { return nil }}
			err := runProviderLaunch(context.Background(), root, LaunchIntent{Provider: "claude"}, hooks, launchTestRuntime())
			if err == nil || errors.Is(err, domain.ErrDeliveryFailed) != failStart {
				t.Fatalf("delivery classification: %v", err)
			}
			if !failStart {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("task exit cause lost: %v", err)
				}
			}
		})
	}
}

func TestPreparedDeliveryFailureReceiptPreservesBothCauses(t *testing.T) {
	recordErr := errors.New("receipt storage unavailable")
	hooks := ProviderLaunchHooks{Record: func(context.Context, ProviderLaunchReceipt) error { return recordErr }}
	for _, cause := range []error{errors.New("task failed"), domain.ErrSelectionChanged, context.Canceled} {
		err := launchFailure(context.Background(), hooks, ProviderLaunchReceipt{}, true, cause)
		if !errors.Is(err, cause) || !errors.Is(err, recordErr) || !errors.Is(err, domain.ErrDeliveryFailed) {
			t.Fatalf("joined delivery cause lost: %v", err)
		}
	}
}
