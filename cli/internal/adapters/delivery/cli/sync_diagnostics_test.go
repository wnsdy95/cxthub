package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
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
	results  []error
}

func (s *diagnosticRetrySync) Push(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	index := len(s.inputs)
	s.contexts = append(s.contexts, ctx)
	s.inputs = append(s.inputs, in)
	var err error
	if index < len(s.results) {
		err = s.results[index]
	}
	end := outbound.BeginSyncDiagnostic(ctx, outbound.SyncStagePush, outbound.SyncDiagnosticCounts{})
	end(err)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	return inbound.SyncOutput{Pushed: 1}, nil
}

type diagnosticOpaqueConflict struct{}

func (diagnosticOpaqueConflict) Error() string { return "publication rejected" }
func (diagnosticOpaqueConflict) Unwrap() error { return domain.ErrSyncConflict }

func TestPrePushDiagnosticsAppendRetrySharesBudget(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CXT_REMOTE", "https://example.invalid")
	t.Setenv("CXT_SYNC_DIAGNOSTICS", "1")
	prior := func(err error) error {
		return fmt.Errorf("previous rejection: %v; terminal: %w", domain.ErrSyncConflict, err)
	}
	auth := func(status int) error {
		return prior(&backendclient.HTTPError{Status: status, Code: "forbidden", Message: "synthetic denial"})
	}
	for _, tc := range []struct {
		name    string
		results []error
		calls   int
	}{
		{"success", nil, 1},
		{"protocol_required", []error{domain.ErrContextProtocolRequired}, 1},
		{"conflict", []error{domain.ErrSyncConflict}, 2},
		{"wrapped_conflict", []error{fmt.Errorf("push: %w", domain.ErrSyncConflict)}, 2},
		{"opaque_conflict", []error{diagnosticOpaqueConflict{}}, 2},
		{"append_conflict", []error{domain.ErrSyncConflict, domain.ErrSyncConflict}, 2},
		{"append_terminal", []error{domain.ErrSyncConflict, auth(403)}, 2},
		{"terminal_401", []error{auth(401)}, 1},
		{"terminal_403", []error{auth(403)}, 1},
		{"terminal_network", []error{prior(&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")})}, 1},
		{"terminal_deadline", []error{prior(context.DeadlineExceeded)}, 1},
		{"branch_text", []error{fmt.Errorf("branch record %q: %w", domain.ErrSyncConflict.Error(), domain.ErrNotFound)}, 1},
		{"plain_text", []error{errors.New(domain.ErrSyncConflict.Error())}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, c, _, repoID, _ := historyFixture(t)
			runLifecycleGit(t, repo, "remote", "add", "origin", "https://example.invalid/code.git")
			c.ResolveRepo = func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repoID}, nil }
			setGitPushInput(t, "refs/heads/main "+gitOut(repo, "rev-parse", "HEAD")+" refs/heads/main "+strings.Repeat("0", 40)+"\n")
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
			syncer := &diagnosticRetrySync{results: tc.results}
			c.Sync = syncer
			c.WakeHistoricalSync = func(string) { wakes++ }
			if err := runGitHook(caller, c, repo, []string{"pre-push", "origin", "https://example.invalid/code.git"}); err != nil {
				t.Fatalf("hook lost fail-open: %v", err)
			}
			if len(syncer.inputs) != tc.calls || wakes != 1 {
				t.Fatalf("push calls=%d want=%d; historical wakeups=%d want=1", len(syncer.inputs), tc.calls, wakes)
			}
			for i, in := range syncer.inputs {
				if in.Append != (i == 1) || !in.ForegroundOnly || in.Force || in.Cwd != repo || in.Publication == nil || in.Publication.HistoryOnly || len(in.Publication.Branches) != 1 || in.Publication.Branches[0].Branch != "main" || in.Publication.Branches[0].BranchID != domain.LegacyContextBranchID(repoID, "main") {
					t.Fatalf("attempt %d changed publication input: %+v", i+1, in)
				}
			}
			wantDeadline, _ := caller.Deadline()
			collector := outbound.SyncDiagnosticsFromContext(syncer.contexts[0])
			if collector == nil {
				t.Fatal("environment opt-in did not attach diagnostics in the real hook")
			}
			for _, attempt := range syncer.contexts {
				deadline, ok := attempt.Deadline()
				if !ok || deadline != wantDeadline || attempt.Done() != syncer.contexts[0].Done() || outbound.SyncDiagnosticsFromContext(attempt) != collector {
					t.Fatal("retry started a new budget, cancellation scope or collector")
				}
			}
			var pushes []outbound.SyncDiagnosticEvent
			for _, event := range collector.Snapshot().Events {
				if event.Stage == outbound.SyncStagePush {
					pushes = append(pushes, event)
				}
			}
			if len(pushes) != tc.calls {
				t.Fatal("push diagnostics lost an attempt")
			}
			for i, push := range pushes {
				if push.Attempt != i+1 || push.OperationID != pushes[0].OperationID || push.StartRemainingMillis == nil {
					t.Fatal("attempt identity or caller budget was lost")
				}
				if i > 0 && *push.StartRemainingMillis > *pushes[i-1].StartRemainingMillis {
					t.Fatal("append retry replenished caller budget")
				}
			}
			raw, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			reports := 0
			if len(tc.results) > 0 && tc.results[0] != nil {
				reports = 1
			}
			if strings.Count(string(raw), "cxt sync diagnostics: ") != reports {
				t.Fatal("diagnostic failure report count changed")
			}
		})
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
