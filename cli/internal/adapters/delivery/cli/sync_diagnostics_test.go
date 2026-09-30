package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestPushDiagnosticsOptInAndSuccessAreSilent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		original, cancel := context.WithTimeout(context.Background(), time.Minute)
		ctx, finish := withPushDiagnostics(original, &output, enabled)
		wantDeadline, _ := original.Deadline()
		if got, _ := ctx.Deadline(); got != wantDeadline {
			t.Fatal("diagnostics changed caller deadline")
		}
		if !enabled && ctx != original {
			t.Fatal("disabled diagnostics must preserve the exact context")
		}
		end := outbound.BeginSyncDiagnostic(ctx, outbound.SyncStagePush, outbound.SyncDiagnosticCounts{})
		end(nil)
		finish()
		finish()
		cancel()
		if output.Len() != 0 {
			t.Fatal("successful push emitted diagnostics")
		}
	}
}

type diagnosticRetrySync struct {
	inbound.SyncRepo
	contexts []context.Context
	inputs   []inbound.SyncInput
}

func (s *diagnosticRetrySync) Push(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.contexts = append(s.contexts, ctx)
	s.inputs = append(s.inputs, in)
	end := outbound.BeginSyncDiagnostic(ctx, outbound.SyncStagePush, outbound.SyncDiagnosticCounts{})
	if len(s.inputs) == 1 {
		end(domain.ErrSyncConflict)
		return inbound.SyncOutput{}, domain.ErrSyncConflict
	}
	end(nil)
	return inbound.SyncOutput{Pushed: 1}, nil
}

func TestPrePushDiagnosticsAppendRetrySharesBudget(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CXT_REMOTE", "https://example.invalid")
	t.Setenv("CXT_SYNC_DIAGNOSTICS", "1")
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "hook-stderr")
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = originalStderr; _ = output.Close() }()
	caller, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wakes := 0
	syncer := &diagnosticRetrySync{}
	c := &Container{Sync: syncer, WakeHistoricalSync: func(string) { wakes++ }}
	if err := runGitHook(caller, c, repo, []string{"pre-push"}); err != nil {
		t.Fatal(err)
	}
	if len(syncer.inputs) != 2 || syncer.inputs[0].Append || !syncer.inputs[1].Append || wakes != 1 {
		t.Fatal("diagnostics changed automatic append or historical wakeup")
	}
	wantDeadline, _ := caller.Deadline()
	collector := outbound.SyncDiagnosticsFromContext(syncer.contexts[0])
	if collector == nil {
		t.Fatal("environment opt-in did not attach diagnostics in the real hook")
	}
	for _, attempt := range syncer.contexts {
		deadline, ok := attempt.Deadline()
		if !ok || deadline != wantDeadline || outbound.SyncDiagnosticsFromContext(attempt) != collector {
			t.Fatal("retry started a new budget or collector")
		}
	}
	var pushes []outbound.SyncDiagnosticEvent
	for _, event := range collector.Snapshot().Events {
		if event.Stage == outbound.SyncStagePush {
			pushes = append(pushes, event)
		}
	}
	if len(pushes) != 2 || pushes[0].Attempt != 1 || pushes[1].Attempt != 2 || pushes[0].OperationID != pushes[1].OperationID || *pushes[1].StartRemainingMillis > *pushes[0].StartRemainingMillis {
		t.Fatal("attempts lost their shared, decreasing caller budget")
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "cxt sync diagnostics: ") != 1 {
		t.Fatal("recovered conflict must retain one observed failure report")
	}
}

func TestPushDiagnosticsFailureReportIsBoundedPrivateAndOnce(t *testing.T) {
	var output bytes.Buffer
	ctx, finish := withPushDiagnostics(context.Background(), &output, true)
	collector := outbound.SyncDiagnosticsFromContext(ctx)
	// The failed event can leave the ring; aggregate evidence must survive.
	outbound.RecordSyncDiagnostic(ctx, outbound.SyncStageHTTPDo, outbound.SyncDiagnosticCounts{}, errors.New("PRIVATE_TOKEN PRIVATE_URL PRIVATE_BODY PRIVATE_CAUSE"))
	for i := 0; i < 200; i++ {
		outbound.RecordSyncDiagnostic(ctx, outbound.SyncStageHTTPBody, outbound.SyncDiagnosticCounts{HTTPStatus: 200}, nil)
	}
	var calls sync.WaitGroup
	for i := 0; i < 8; i++ {
		calls.Go(finish)
	}
	calls.Wait()
	if strings.Count(output.String(), "cxt sync diagnostics: ") != 1 {
		t.Fatal("failure summary was not emitted exactly once")
	}
	for _, forbidden := range []string{"PRIVATE_TOKEN", "PRIVATE_URL", "PRIVATE_BODY", "PRIVATE_CAUSE"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("report exposed %s", forbidden)
		}
	}
	var report outbound.SyncDiagnosticsReport
	raw := strings.TrimSpace(strings.TrimPrefix(output.String(), "cxt sync diagnostics: "))
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if !report.HasFailures || len(report.Events) != 64 || report.DroppedCount != 137 || report.OperationID != collector.Snapshot().OperationID {
		t.Fatal("bounded report lost aggregate failure evidence")
	}
}

type failingSyncDiagnosticWriter struct{ writes int }

func (w *failingSyncDiagnosticWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("diagnostic destination unavailable")
}

func TestPushDiagnosticsOutputFailureDoesNotChangeCaller(t *testing.T) {
	original, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("PRIVATE_CAUSE")
	writer := &failingSyncDiagnosticWriter{}
	ctx, finish := withPushDiagnostics(original, writer, true)
	cancel(cause)
	outbound.RecordSyncDiagnostic(ctx, outbound.SyncStagePush, outbound.SyncDiagnosticCounts{}, cause)
	finish()
	finish()
	if writer.writes != 1 || ctx.Err() != context.Canceled || context.Cause(ctx) != cause {
		t.Fatal("reporting changed cancellation or retried a failed output")
	}
}

func TestPushDiagnosticsNestedEntryKeepsOriginalCollector(t *testing.T) {
	var outer, inner bytes.Buffer
	ctx, finish := withPushDiagnostics(context.Background(), &outer, true)
	collector := outbound.SyncDiagnosticsFromContext(ctx)
	nested, finishNested := withPushDiagnostics(ctx, &inner, true)
	if nested != ctx || outbound.SyncDiagnosticsFromContext(nested) != collector {
		t.Fatal("nested entry restarted diagnostics")
	}
	outbound.RecordSyncDiagnostic(nested, outbound.SyncStagePush, outbound.SyncDiagnosticCounts{}, errors.New("private"))
	finishNested()
	finish()
	if inner.Len() != 0 || outer.Len() == 0 {
		t.Fatal("nested entry took ownership of the caller report")
	}
}
