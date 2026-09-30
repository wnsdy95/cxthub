package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reading this error's text would itself fail the test.
type syncDiagnosticSecretError struct{}

func (syncDiagnosticSecretError) Error() string { panic("secret error text was read") }

type syncDiagnosticWrappedError struct{ err error }

func (syncDiagnosticWrappedError) Error() string   { panic("secret wrapper text was read") }
func (e syncDiagnosticWrappedError) Unwrap() error { return e.err }

type syncDiagnosticTimeoutError struct{ syncDiagnosticSecretError }

func (syncDiagnosticTimeoutError) Timeout() bool { return true }

type syncDiagnosticDeadlineTimeoutError struct{ syncDiagnosticWrappedError }

func (syncDiagnosticDeadlineTimeoutError) Timeout() bool { return true }

func syncDiagnosticOnlyEvent(t *testing.T, d *SyncDiagnostics) SyncDiagnosticEvent {
	t.Helper()
	r := d.Snapshot()
	if len(r.Events) != 1 {
		t.Fatalf("got %d events, want one", len(r.Events))
	}
	return r.Events[0]
}

func TestSyncDiagnosticsBoundsAndEviction(t *testing.T) {
	for _, tc := range []struct{ limit, want int }{{-1, 64}, {0, 64}, {1, 1}, {3, 3}, {128, 128}, {129, 128}, {int(^uint(0) >> 1), 128}} {
		t.Run(strconv.Itoa(tc.limit), func(t *testing.T) {
			d := NewSyncDiagnostics(tc.limit)
			ctx := WithSyncDiagnostics(context.Background(), d)
			RecordSyncDiagnostic(ctx, SyncStageHTTPDo, SyncDiagnosticCounts{Bytes: 7}, syncDiagnosticSecretError{})
			for i := 1; i < tc.want+10; i++ {
				RecordSyncDiagnostic(ctx, SyncStageHTTPDo, SyncDiagnosticCounts{Bytes: 7}, nil)
			}
			r := d.Snapshot()
			if len(r.Events) != tc.want || r.DroppedCount != 10 || !r.HasFailures || !d.HasFailures() {
				t.Fatalf("unexpected retention/failure state: %+v", r)
			}
			for i, event := range r.Events {
				if event.Sequence != uint64(11+i) || event.OperationID != r.OperationID || event.Outcome != SyncDiagnosticSuccess || event.DurationMillis != 0 {
					t.Fatalf("unexpected retained event: %+v", event)
				}
			}
			if len(r.Totals) != 1 || r.Totals[0].Calls != uint64(tc.want+10) || r.Totals[0].Failures != 1 || r.Totals[0].Bytes != int64((tc.want+10)*7) {
				t.Fatalf("evicted observations missing from totals: %+v", r.Totals)
			}
		})
	}
}

func TestSyncDiagnosticsAbsentAndDisabled(t *testing.T) {
	ctx := context.Background()
	for _, absent := range []context.Context{ctx, WithSyncDiagnostics(ctx, nil), nil} {
		if SyncDiagnosticsFromContext(absent) != nil || WithSyncDiagnosticScope(absent, SyncRoleInventory, 1, 2) != absent ||
			WithSyncDiagnosticRole(absent, SyncRoleDocumentChunks) != absent || WithSyncDiagnosticDocument(absent, 2) != absent ||
			WithSyncDiagnosticAttempt(absent, 2) != absent || NextSyncDiagnosticRequest(absent) != absent {
			t.Fatal("disabled helpers did not preserve the original context")
		}
		end := BeginSyncDiagnostic(absent, SyncStagePush, SyncDiagnosticCounts{})
		end(syncDiagnosticSecretError{})
		end(nil)
		RecordSyncDiagnostic(absent, SyncStagePush, SyncDiagnosticCounts{}, syncDiagnosticSecretError{})
	}
	d := NewSyncDiagnostics(1)
	enabled := WithSyncDiagnostics(ctx, d)
	disabled := WithSyncDiagnostics(enabled, nil)
	RecordSyncDiagnostic(disabled, SyncStagePush, SyncDiagnosticCounts{}, syncDiagnosticSecretError{})
	if len(d.Snapshot().Events) != 0 || d.HasFailures() {
		t.Fatal("nil attachment did not disable inherited collection")
	}
	var absent *SyncDiagnostics
	if absent.HasFailures() || len(absent.Snapshot().Events) != 0 {
		t.Fatal("nil collector is not empty")
	}
	if _, err := json.Marshal(absent.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDiagnosticScopeOwnershipAndContextPreservation(t *testing.T) {
	type contextValueKey struct{}
	caller, cancel := context.WithTimeout(context.WithValue(context.Background(), contextValueKey{}, "private value"), time.Hour)
	defer cancel()
	d := NewSyncDiagnostics(16)
	ctx := WithSyncDiagnostics(caller, d)
	ctx = WithSyncDiagnosticScope(ctx, SyncRoleInventory, 2, 3)
	ctx = NextSyncDiagnosticRequest(ctx)
	role := WithSyncDiagnosticRole(ctx, SyncRoleDocumentChunks)
	document := WithSyncDiagnosticDocument(role, 9)
	attempt := WithSyncDiagnosticAttempt(document, 4)
	for _, derived := range []context.Context{ctx, role, document, attempt, NextSyncDiagnosticRequest(attempt)} {
		deadline, ok := derived.Deadline()
		original, _ := caller.Deadline()
		if !ok || deadline != original || derived.Done() != caller.Done() || derived.Err() != caller.Err() || derived.Value(contextValueKey{}) != "private value" || SyncDiagnosticsFromContext(derived) != d {
			t.Fatal("diagnostics changed the caller context")
		}
	}
	for _, derived := range []context.Context{ctx, role, document, attempt} {
		RecordSyncDiagnostic(derived, SyncStagePush, SyncDiagnosticCounts{}, nil)
	}
	events := d.Snapshot().Events
	wantScopes := []syncDiagnosticScope{{SyncRoleInventory, 2, 3}, {SyncRoleDocumentChunks, 2, 3}, {SyncRoleDocumentChunks, 2, 9}, {SyncRoleDocumentChunks, 4, 9}}
	for i, want := range wantScopes {
		if event := events[i]; event.Role != want.role || event.Attempt != want.attempt || event.Document != want.document || event.Request != 1 {
			t.Fatalf("scope %d was not preserved: %+v", i, event)
		}
	}
	RecordSyncDiagnostic(NextSyncDiagnosticRequest(ctx), SyncStagePush, SyncDiagnosticCounts{}, nil)
	if d.Snapshot().Events[4].Request != 3 {
		t.Fatal("request allocation was not operation-wide")
	}
	other := NewSyncDiagnostics(1)
	RecordSyncDiagnostic(WithSyncDiagnostics(ctx, other), SyncStageRetention, SyncDiagnosticCounts{}, nil)
	if len(d.Snapshot().Events) != 5 || other.Snapshot().OperationID == d.Snapshot().OperationID {
		t.Fatal("collector ownership or operation IDs overlap")
	}
	if syncDiagnosticOnlyEvent(t, other).Request != 0 {
		t.Fatal("replacement collector inherited another operation's request number")
	}
	cancel()
	if ctx.Err() != context.Canceled || attempt.Err() != context.Canceled {
		t.Fatal("derived cancellation did not match caller")
	}
}

func TestSyncDiagnosticsSanitizeAndSnapshotOwnership(t *testing.T) {
	const secret = "token-secret /private/path https://private.example body identity"
	caller, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	d := NewSyncDiagnostics(8)
	ctx := WithSyncDiagnosticScope(WithSyncDiagnostics(caller, d), SyncDiagnosticRole(secret), -1, -2)
	ctx = WithSyncDiagnosticRole(ctx, SyncDiagnosticRole(secret))
	ctx = WithSyncDiagnosticDocument(ctx, -3)
	ctx = WithSyncDiagnosticAttempt(ctx, -4)
	RecordSyncDiagnostic(ctx, SyncDiagnosticStage(secret), SyncDiagnosticCounts{-1, -2, -3, -4, -5, -6}, syncDiagnosticSecretError{})
	r := d.Snapshot()
	event := syncDiagnosticOnlyEvent(t, d)
	if event.Stage != SyncStageUnknown || event.Role != SyncRoleOther || event.Attempt != 0 || event.Document != 0 || event.Counts != (SyncDiagnosticCounts{}) || event.Outcome != SyncDiagnosticError {
		t.Fatalf("unsanitized event: %+v", event)
	}
	encoded, err := json.Marshal(r)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("unsafe JSON: %s; error %v", encoded, err)
	}
	if event.StartRemainingMillis == nil || event.EndRemainingMillis == nil {
		t.Fatal("caller deadline missing")
	}
	r.Events[0].Stage = SyncStagePush
	*r.Events[0].StartRemainingMillis = -123
	*r.Events[0].EndRemainingMillis = -456
	r.Totals[0].Bytes = 100
	after := d.Snapshot()
	if after.Events[0].Stage != SyncStageUnknown || *after.Events[0].StartRemainingMillis == -123 || *after.Events[0].EndRemainingMillis == -456 || after.Totals[0].Bytes != 0 {
		t.Fatal("snapshot mutation reached the collector")
	}
	var decoded SyncDiagnosticsReport
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Events[0].Outcome != SyncDiagnosticError {
		t.Fatalf("report is not JSON round-trip safe: %v", err)
	}
}

func TestSyncDiagnosticsStagesAndCounts(t *testing.T) {
	stages := []SyncDiagnosticStage{SyncStageHookReplay, SyncStagePush, SyncStageRetention, SyncStagePreparation,
		SyncStageDocumentRead, SyncStageCanonical, SyncStageChunkPlan, SyncStageRequestMarshal, SyncStageRequestSetup,
		SyncStageTokenLookup, SyncStageHTTPDo, SyncStageHTTPBody, SyncStageHTTPGotConn, SyncStageHTTPWroteRequest, SyncStageHTTPFirstByte, SyncStageUnknown}
	d := NewSyncDiagnostics(32)
	ctx := WithSyncDiagnostics(context.Background(), d)
	counts := SyncDiagnosticCounts{Snapshots: 1, Documents: 2, Chunks: 3, Bytes: 4, HTTPStatus: 503, ClientTimeoutMillis: 5000}
	for _, stage := range stages {
		RecordSyncDiagnostic(ctx, stage, counts, nil)
	}
	r := d.Snapshot()
	if len(r.Totals) != 16 || r.HasFailures {
		t.Fatalf("unexpected fixed-stage totals: %+v", r)
	}
	for i, event := range r.Events {
		if event.Stage != stages[i] || event.Counts != counts || event.StartRemainingMillis != nil || event.EndRemainingMillis != nil || event.Role != SyncRoleOther {
			t.Fatalf("stage/counts/deadline mismatch: %+v", event)
		}
		total := r.Totals[i]
		if total.Stage != stages[i] || total.Calls != 1 || total.Failures != 0 || total.Snapshots != 1 || total.Documents != 2 || total.Chunks != 3 || total.Bytes != 4 {
			t.Fatalf("totals mismatch: %+v", total)
		}
	}
}

func TestSyncDiagnosticsErrorEvidence(t *testing.T) {
	t.Run("deadline before Do", func(t *testing.T) {
		caller, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		d := NewSyncDiagnostics(1)
		ctx := WithSyncDiagnostics(caller, d)
		BeginSyncDiagnostic(ctx, SyncStageHTTPDo, SyncDiagnosticCounts{})(syncDiagnosticWrappedError{context.DeadlineExceeded})
		e := syncDiagnosticOnlyEvent(t, d)
		if e.Outcome != SyncDiagnosticDeadlineBeforeDo || e.StartCallerState != SyncContextDeadline || e.EndCallerState != SyncContextDeadline || e.StartRemainingMillis == nil || *e.StartRemainingMillis >= 0 {
			t.Fatalf("pre-Do deadline evidence lost: %+v", e)
		}
	})
	t.Run("caller deadline during Do", func(t *testing.T) {
		caller, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		d := NewSyncDiagnostics(1)
		end := BeginSyncDiagnostic(WithSyncDiagnostics(caller, d), SyncStageHTTPDo, SyncDiagnosticCounts{})
		<-caller.Done()
		end(syncDiagnosticWrappedError{context.DeadlineExceeded})
		e := syncDiagnosticOnlyEvent(t, d)
		if e.Outcome != SyncDiagnosticDeadline || e.StartCallerState != SyncContextActive || e.EndCallerState != SyncContextDeadline || e.DurationMillis < 1 || *e.EndRemainingMillis > *e.StartRemainingMillis {
			t.Fatalf("during-Do deadline evidence lost: %+v", e)
		}
		if d.Snapshot().Totals[0].DurationMillis != e.DurationMillis {
			t.Fatal("duration missing from aggregate")
		}
	})
	t.Run("caller cancellation and custom cause", func(t *testing.T) {
		caller, cancel := context.WithCancelCause(context.Background())
		d := NewSyncDiagnostics(1)
		end := BeginSyncDiagnostic(WithSyncDiagnostics(caller, d), SyncStageHTTPBody, SyncDiagnosticCounts{})
		cancel(syncDiagnosticSecretError{})
		end(syncDiagnosticSecretError{})
		e := syncDiagnosticOnlyEvent(t, d)
		if e.Outcome != SyncDiagnosticCanceled || e.StartCallerState != SyncContextActive || e.EndCallerState != SyncContextCanceled {
			t.Fatalf("cancellation evidence lost: %+v", e)
		}
	})
	t.Run("derived timeout with active original caller", func(t *testing.T) {
		d := NewSyncDiagnostics(1)
		ctx := WithSyncDiagnostics(context.Background(), d)
		child, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		end := BeginSyncDiagnostic(child, SyncStageHTTPDo, SyncDiagnosticCounts{ClientTimeoutMillis: 20})
		<-child.Done()
		end(syncDiagnosticWrappedError{context.DeadlineExceeded})
		e := syncDiagnosticOnlyEvent(t, d)
		if e.Outcome != SyncDiagnosticTimeoutActive || e.StartCallerState != SyncContextActive || e.EndCallerState != SyncContextActive || e.EndContextState != SyncContextDeadline || e.StartRemainingMillis != nil {
			t.Fatalf("derived timeout confused with caller deadline: %+v", e)
		}
	})
	for _, tc := range []struct {
		name string
		err  error
		want SyncDiagnosticErrorClass
	}{
		{"wrapped deadline with active contexts", syncDiagnosticWrappedError{context.DeadlineExceeded}, SyncDiagnosticTimeoutActive},
		{"private client timeout with active contexts", syncDiagnosticDeadlineTimeoutError{syncDiagnosticWrappedError{context.DeadlineExceeded}}, SyncDiagnosticTimeoutActive},
		{"wrapped cancel", syncDiagnosticWrappedError{context.Canceled}, SyncDiagnosticCanceled},
		{"timeout active", syncDiagnosticWrappedError{syncDiagnosticTimeoutError{}}, SyncDiagnosticTimeoutActive},
		{"generic", syncDiagnosticSecretError{}, SyncDiagnosticError},
		{"success", nil, SyncDiagnosticSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSyncDiagnostics(1)
			RecordSyncDiagnostic(WithSyncDiagnostics(context.Background(), d), SyncStageHTTPDo, SyncDiagnosticCounts{}, tc.err)
			e := syncDiagnosticOnlyEvent(t, d)
			if e.Outcome != tc.want || d.HasFailures() != (tc.err != nil) {
				t.Fatalf("outcome %s, want %s", e.Outcome, tc.want)
			}
		})
	}
}

func TestSyncDiagnosticsNilErrorAndCleanupCancellation(t *testing.T) {
	d := NewSyncDiagnostics(8)
	ctx := WithSyncDiagnostics(context.Background(), d)
	child, cancel := context.WithCancel(ctx)
	end := BeginSyncDiagnostic(child, SyncStageHTTPBody, SyncDiagnosticCounts{})
	cancel()
	end(nil)
	RecordSyncDiagnostic(ctx, SyncStagePush, SyncDiagnosticCounts{HTTPStatus: 503}, nil)
	for _, event := range d.Snapshot().Events {
		if event.Outcome != SyncDiagnosticSuccess || event.EndCallerState != SyncContextActive {
			t.Fatalf("nil result invented a failure: %+v", event)
		}
	}
	if d.HasFailures() {
		t.Fatal("nil business results counted as failures")
	}
	caller, cancelCaller := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelCaller()
	RecordSyncDiagnostic(WithSyncDiagnostics(caller, d), SyncStageHTTPDo, SyncDiagnosticCounts{}, nil)
	if d.HasFailures() || d.Snapshot().Events[2].Outcome != SyncDiagnosticSuccess {
		t.Fatal("expired caller with a nil result invented a failure")
	}
}

func TestSyncDiagnosticsExactlyOnceAndReleasedContexts(t *testing.T) {
	d := NewSyncDiagnostics(8)
	ctx := WithSyncDiagnostics(context.Background(), d)
	active := beginSyncDiagnostic(ctx, SyncStageCanonical, SyncDiagnosticCounts{Bytes: 11})
	active.finish(syncDiagnosticSecretError{})
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Go(func() { active.finish(nil) })
	}
	group.Wait()
	if active.ctx != nil || active.caller != nil || active.collector != nil {
		t.Fatal("completed callback retains context or collector")
	}
	e := syncDiagnosticOnlyEvent(t, d)
	if e.Outcome != SyncDiagnosticError || d.Snapshot().Totals[0].Calls != 1 || d.Snapshot().Totals[0].Bytes != 11 {
		t.Fatal("completion was recorded more than once or winning outcome changed")
	}
}

func TestSyncDiagnosticsConcurrentCollection(t *testing.T) {
	d := NewSyncDiagnostics(128)
	ctx := WithSyncDiagnostics(context.Background(), d)
	gate := make(chan struct{})
	var workers, readers sync.WaitGroup
	for i := 0; i < 96; i++ {
		workers.Go(func() {
			<-gate
			request := NextSyncDiagnosticRequest(ctx)
			end := BeginSyncDiagnostic(request, SyncStageHTTPDo, SyncDiagnosticCounts{Bytes: 1})
			var completions sync.WaitGroup
			for j := 0; j < 4; j++ {
				completions.Go(func() { end(nil) })
			}
			completions.Wait()
		})
	}
	for i := 0; i < 4; i++ {
		readers.Go(func() {
			<-gate
			for j := 0; j < 100; j++ {
				r := d.Snapshot()
				if _, err := json.Marshal(r); err != nil {
					t.Error(err)
				}
				_ = d.HasFailures()
				for k := range r.Events {
					r.Events[k].Counts.Bytes = 200
				}
			}
		})
	}
	close(gate)
	workers.Wait()
	readers.Wait()
	r := d.Snapshot()
	if len(r.Events) != 96 || r.DroppedCount != 0 || r.HasFailures || len(r.Totals) != 1 || r.Totals[0].Calls != 96 || r.Totals[0].Bytes != 96 {
		t.Fatalf("concurrent recording lost or duplicated events: %+v", r)
	}
	seen := make(map[uint64]bool)
	for i, event := range r.Events {
		if seen[event.Request] || event.Request == 0 || event.Request > 96 || event.Sequence != uint64(i+1) {
			t.Fatalf("invalid concurrent sequence/request: %+v", event)
		}
		seen[event.Request] = true
	}
}

func TestSyncDiagnosticsZeroValueAndSaturatingTotals(t *testing.T) {
	var d SyncDiagnostics
	ctx := WithSyncDiagnostics(context.Background(), &d)
	const largest = int64(1<<63 - 1)
	for i := 0; i < 70; i++ {
		RecordSyncDiagnostic(ctx, SyncStageHTTPBody, SyncDiagnosticCounts{Bytes: int(^uint(0) >> 1)}, nil)
	}
	r := d.Snapshot()
	if len(r.Events) != 64 || r.DroppedCount != 6 || r.OperationID == "" || r.Totals[0].Bytes < 0 {
		t.Fatalf("zero-value collector or totals overflow: %+v", r)
	}
	if syncDiagnosticAdd(largest-1, 2) != largest {
		t.Fatal("total did not saturate")
	}
}

func TestSyncDiagnosticsInstantNeverReadsErrorText(t *testing.T) {
	d := NewSyncDiagnostics(1)
	ctx := WithSyncDiagnostics(context.Background(), d)
	RecordSyncDiagnostic(ctx, SyncStagePush, SyncDiagnosticCounts{}, errors.Join(syncDiagnosticSecretError{}, context.Canceled))
	if syncDiagnosticOnlyEvent(t, d).Outcome != SyncDiagnosticCanceled {
		t.Fatal("joined cancellation was not recognized")
	}
	if _, err := json.Marshal(d.Snapshot()); err != nil {
		t.Fatal(err)
	}
	const secret = "private-token https://user:password@example.test/private body"
	RecordSyncDiagnostic(ctx, SyncStageHTTPBody, SyncDiagnosticCounts{}, errors.Join(errors.New(secret), context.Canceled))
	encoded, err := json.Marshal(d.Snapshot())
	if err != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "private-token") {
		t.Fatalf("error text escaped into report: %s; error %v", encoded, err)
	}
}

func TestSyncDiagnosticMeasuredCompletionCountsOnceUnderConcurrency(t *testing.T) {
	collector := NewSyncDiagnostics(2)
	ctx := WithSyncDiagnostics(context.Background(), collector)
	end := BeginMeasuredSyncDiagnostic(ctx, SyncStageCanonical)
	var calls sync.WaitGroup
	for _, count := range []int{10, 20} {
		calls.Go(func() { end(SyncDiagnosticCounts{Bytes: count}, nil) })
	}
	calls.Wait()
	report := collector.Snapshot()
	if len(report.Events) != 1 || len(report.Totals) != 1 || report.Totals[0].Calls != 1 {
		t.Fatal("measured completion produced multiple calls")
	}
	bytes := report.Events[0].Counts.Bytes
	if (bytes != 10 && bytes != 20) || report.Totals[0].Bytes != int64(bytes) {
		t.Fatal("final counts did not belong to the winning completion")
	}
	end(SyncDiagnosticCounts{Bytes: 99}, errors.New("private"))
	if after := collector.Snapshot(); after.Totals[0] != report.Totals[0] || after.HasFailures {
		t.Fatal("late completion changed counts or the acknowledged outcome")
	}
}
