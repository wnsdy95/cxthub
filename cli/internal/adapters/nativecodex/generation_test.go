//go:build darwin || linux

package nativecodex

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationDefersInjectionUntilExactQuestionAndRecordsOnce(t *testing.T) {
	s, trace := startFixture(t, "generation")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thread, err := s.StartThread(ctx, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var prepares, validations, records atomic.Int32
	h, err := s.OpenGenerationHandoff(ctx, func(ctx context.Context, got Thread, prompt string) (PreparedGeneration, error) {
		prepares.Add(1)
		if got != thread || prompt != "PRIVATE_INITIAL_QUESTION" {
			return PreparedGeneration{}, errors.New("private mismatch")
		}
		return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "PRIVATE_LATEST_MAIN"}}, Validate: func(context.Context) error { validations.Add(1); return nil }, Observe: func(_ context.Context, o GenerationObservation) error {
			records.Add(1)
			if o.TotalInputTokens != 120 {
				return errors.New("unexpected usage")
			}
			return nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}); err != nil {
		t.Fatal(err)
	}
	ready, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if prepares.Load() != 0 || ready.PayloadHash != "" || ready.ThreadID != thread.ID {
		t.Fatal("preparation ran before initial question")
	}
	raw, _ := os.ReadFile(trace)
	if strings.Contains(string(raw), "thread/inject_items") {
		t.Fatal("early injection")
	}
	if _, err = rpc.call(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "PRIVATE_INITIAL_QUESTION"}}}); err != nil {
		t.Fatal(err)
	}
	o, err := h.WaitGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.Outcome != "completed" || !o.UsageKnown || !o.ExecutionStarted || o.TotalInputTokens != 120 || prepares.Load() != 1 || validations.Load() != 2 || records.Load() != 1 {
		t.Fatalf("observation=%+v prepare=%d validate=%d record=%d", o, prepares.Load(), validations.Load(), records.Load())
	}
	if _, err = rpc.call(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "second"}}}); err != nil {
		t.Fatal(err)
	}
	if prepares.Load() != 1 || records.Load() != 1 {
		t.Fatal("first preparation repeated")
	}
	raw, _ = os.ReadFile(trace)
	tr := string(raw)
	if strings.Count(tr, "thread/inject_items") != 1 || strings.Index(tr, "thread/inject_items") > strings.Index(tr, "turn/start") {
		t.Fatal("injection order")
	}
}

func TestGenerationStopsBeforeRequestOnPreparationOrValidationFailure(t *testing.T) {
	for _, fail := range []string{"prepare", "before injection", "after injection", "cancel"} {
		t.Run(fail, func(t *testing.T) {
			s, trace := startFixture(t, "generation")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			thread, err := s.StartThread(ctx, ThreadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			h, err := s.OpenGenerationHandoff(ctx, func(ctx context.Context, _ Thread, _ string) (PreparedGeneration, error) {
				if fail == "prepare" {
					return PreparedGeneration{}, errors.New("PRIVATE_FAILURE")
				}
				if fail == "cancel" {
					cancel()
					return PreparedGeneration{}, ctx.Err()
				}
				return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "private"}}, Validate: func(context.Context) error {
					count++
					if fail == "before injection" || count == 2 {
						return errors.New("PRIVATE_FAILURE")
					}
					return nil
				}, Observe: func(context.Context, GenerationObservation) error { return nil }}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			c, err := dialHandoff(t, h)
			if err != nil {
				t.Fatal(err)
			}
			rpc := initializeHandoff(t, c)
			if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}); err != nil {
				t.Fatal(err)
			}
			_, err = rpc.call(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "q"}}})
			if err == nil || strings.Contains(err.Error(), "PRIVATE_FAILURE") {
				t.Fatal("failure leaked or released")
			}
			raw, _ := os.ReadFile(trace)
			if strings.Contains(string(raw), "turn/start") {
				t.Fatal("generation escaped failed gate")
			}
			if fail != "after injection" && strings.Contains(string(raw), "thread/inject_items") {
				t.Fatal("injection escaped failed gate")
			}
		})
	}
}

func TestGenerationRequiresFreshDurableMetadata(t *testing.T) {
	for _, mode := range []string{"materialize-invalid", "materialize-not-fresh"} {
		t.Run(mode, func(t *testing.T) {
			s, trace := startFixture(t, mode)
			ctx := context.Background()
			if _, err := s.StartThread(ctx, ThreadOptions{}); err != nil {
				t.Fatal(err)
			}
			prepare := func(context.Context, Thread, string) (PreparedGeneration, error) {
				t.Fatal("unexpected preparation")
				return PreparedGeneration{}, nil
			}
			if _, err := s.OpenGenerationHandoff(ctx, prepare); err == nil {
				t.Fatal("invalid persisted thread admitted")
			}
			if _, err := s.OpenGenerationHandoff(ctx, prepare); err == nil {
				t.Fatal("uncertain initialization retried")
			}
			data, _ := os.ReadFile(trace)
			if strings.Contains(string(data), "thread/inject_items") || strings.Contains(string(data), "turn/start") {
				t.Fatal("failed metadata initialization generated history")
			}
		})
	}
	s, trace := startFixture(t, "generation")
	if _, err := s.StartThread(context.Background(), ThreadOptions{Ephemeral: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenGenerationHandoff(context.Background(), func(context.Context, Thread, string) (PreparedGeneration, error) { return PreparedGeneration{}, nil }); err == nil {
		t.Fatal("ephemeral thread admitted")
	}
	data, _ := os.ReadFile(trace)
	if strings.Contains(string(data), "thread/section/move") {
		t.Fatal("ephemeral thread materialized")
	}
}

type fixtureCalibrationError struct{}

func (fixtureCalibrationError) Error() string                       { return "PRIVATE_STORE_ERROR" }
func (fixtureCalibrationError) CalibrationPersistenceFailure() bool { return true }

func TestGenerationFeedbackOutageDoesNotDisconnectOrRetry(t *testing.T) {
	s, trace := startFixture(t, "generation")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thread, err := s.StartThread(ctx, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var observations atomic.Int32
	h, err := s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
		return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "history"}}, Validate: func(context.Context) error { return nil }, Observe: func(context.Context, GenerationObservation) error {
			observations.Add(1)
			return fixtureCalibrationError{}
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "question"}}}
	if _, err = rpc.call(ctx, "turn/start", params); err != nil {
		t.Fatal(err)
	}
	o, err := h.WaitGeneration(ctx)
	if !errors.Is(err, ErrCalibrationPersistence) || strings.Contains(err.Error(), "PRIVATE") || o.Outcome != "completed" {
		t.Fatal("feedback failure confused with model outcome", err)
	}
	if _, err = rpc.call(ctx, "turn/start", params); err != nil {
		t.Fatal("cache outage disconnected conversation", err)
	}
	data, _ := os.ReadFile(trace)
	if observations.Load() != 1 || strings.Count(string(data), "thread/inject_items") != 1 || strings.Count(string(data), "turn/start") != 2 {
		t.Fatal("ambiguous observation retried")
	}
}

func TestGenerationFastQuestionPreservesInjectionReceipt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handoff{ctx: ctx, cancel: cancel, ready: make(chan struct{})}
	h.startup = time.AfterFunc(time.Hour, func() {})
	defer h.startup.Stop()
	// Deterministic ordering: forwarding resume wakes the client; its complete
	// preparation stores the hash before the response relay records readiness.
	h.receipt.PayloadHash = "sha256:prepared-before-resume-record"
	h.completeResume(HandoffReceipt{ThreadID: "owned-thread", ResumeAcknowledged: true, ProviderAcceptance: "unverified"})
	if h.receipt.ThreadID != "owned-thread" || h.receipt.PayloadHash != "sha256:prepared-before-resume-record" {
		t.Fatal("resume erased completed injection")
	}
}

func TestGenerationClientDisconnectCancelsBlockedPreparation(t *testing.T) {
	s, trace := startFixture(t, "generation")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thread, err := s.StartThread(ctx, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled := make(chan struct{}), make(chan struct{})
	h, err := s.OpenGenerationHandoff(ctx, func(ctx context.Context, _ Thread, _ string) (PreparedGeneration, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		// Even a late callback that returns a package cannot release it.
		return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "must not inject"}}, Validate: func(context.Context) error { return nil }, Observe: func(context.Context, GenerationObservation) error { return nil }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}); err != nil {
		t.Fatal(err)
	}
	if err = rpc.write(ctx, new(uint64), "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "q"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("preparation not entered")
	}
	_ = c.CloseNow()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client close did not cancel preparation")
	}
	_ = h.Close()
	data, _ := os.ReadFile(trace)
	if strings.Contains(string(data), "thread/inject_items") || strings.Contains(string(data), "turn/start") {
		t.Fatal("disconnected client released model input")
	}
}
