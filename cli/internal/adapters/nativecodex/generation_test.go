//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

func TestGenerationLifecycleWaitSurvivesFirstSuccess(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprint(closeSession), func(t *testing.T) {
			s, _ := startFixture(t, "generation")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			thread, err := s.StartThread(ctx, ThreadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
				return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: "PRIVATE_HISTORY"}}, Validate: func(context.Context) error { return nil }, Observe: func(context.Context, GenerationObservation) error { return nil }}, nil
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
			if _, err := rpc.call(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}); err != nil {
				t.Fatal(err)
			}
			waiter, stopWaiting := context.WithCancel(ctx)
			stopWaiting()
			if !errors.Is(h.WaitLifecycle(waiter), context.Canceled) {
				t.Fatal("waiter cancellation not honored")
			}
			done := make(chan error, 1)
			go func() { done <- h.WaitLifecycle(ctx) }()
			for _, question := range []string{"first", "second"} {
				if _, err := rpc.call(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": question}}}); err != nil {
					t.Fatal("waiter cancellation or first success ended runtime", err)
				}
				if o, err := h.WaitGeneration(ctx); err != nil || o.Outcome != "completed" {
					t.Fatal("first-turn result unavailable", err)
				}
				select {
				case err := <-done:
					t.Fatal("historical result ended lifecycle wait", err)
				default:
				}
			}
			if closeSession {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = c.CloseNow()
			}
			select {
			case err := <-done:
				if err == nil || ctx.Err() != nil {
					t.Fatal("closure did not terminate lifecycle", err)
				}
			case <-ctx.Done():
				t.Fatal("closure left lifecycle wait running")
			}
		})
	}
}

func TestGenerationStopsBeforeRequestOnPreparationOrValidationFailure(t *testing.T) {
	for _, fail := range []string{"prepare", "before injection", "injection ACK", "after injection", "cancel"} {
		t.Run(fail, func(t *testing.T) {
			mode := "generation"
			if fail == "injection ACK" {
				mode = "bad-ack"
			}
			s, trace := startFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			thread, err := s.StartThread(ctx, ThreadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			var persistenceCalls atomic.Int32
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
				}, BeforeRelease: func(context.Context, InjectionReceipt) error {
					persistenceCalls.Add(1)
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
			if fail != "after injection" && fail != "injection ACK" && strings.Contains(string(raw), "thread/inject_items") {
				t.Fatal("injection escaped failed gate")
			}
			if persistenceCalls.Load() != 0 {
				t.Fatal("persistence ran before successful final validation")
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

func TestGenerationBeforeReleasePersistsOnceAndRevalidates(t *testing.T) {
	s, trace := startFixture(t, "generation")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	thread, err := s.StartThread(ctx, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(t.TempDir(), "receipt.json")
	var validations, writes atomic.Int32
	h, err := s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
		return PreparedGeneration{
			History: []HistoryMessage{{Role: "user", Text: "PRIVATE_ARCHIVED_TEXT"}},
			Validate: func(context.Context) error {
				count := validations.Add(1)
				if count == 3 {
					if _, err := os.ReadFile(receiptPath); err != nil || writes.Load() != 1 {
						return errors.New("receipt was not persisted before revalidation")
					}
				}
				return nil
			},
			BeforeRelease: func(ctx context.Context, receipt InjectionReceipt) error {
				writes.Add(1)
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > generationPersistenceTimeout || ctx.Err() != nil {
					return errors.New("persistence context is not bounded")
				}
				methods, err := os.ReadFile(trace)
				if err != nil || strings.Count(string(methods), "thread/inject_items") != 1 || strings.Contains(string(methods), "turn/start") || validations.Load() != 2 {
					return errors.New("persistence was not between validated injection and generation")
				}
				if receipt != s.injection || !receipt.Acknowledged || receipt.ThreadID != thread.ID || receipt.ProviderAcceptance != "unverified" {
					return errors.New("persistence did not receive the native injection receipt")
				}
				f, err := os.OpenFile(receiptPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return err
				}
				defer f.Close()
				if err = json.NewEncoder(f).Encode(receipt); err != nil {
					return err
				}
				return f.Sync()
			},
			Observe: func(context.Context, GenerationObservation) error { return nil },
		}, nil
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
	if writes.Load() != 0 {
		t.Fatal("persistence ran during inspection")
	}
	params := map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "PRIVATE_QUESTION"}}}
	if _, err = rpc.call(ctx, "turn/start", params); err != nil {
		t.Fatal(err)
	}
	if _, err = h.WaitGeneration(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = rpc.call(ctx, "turn/start", params); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || validations.Load() != 3 {
		t.Fatalf("persistence=%d validation=%d", writes.Load(), validations.Load())
	}
	raw, err := os.ReadFile(receiptPath)
	if err != nil || strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("receipt missing or private text persisted", err)
	}
	h.mu.Lock()
	retained := h.prepared.BeforeRelease != nil
	h.mu.Unlock()
	if retained {
		t.Fatal("completed persistence callback retained")
	}
}

func TestGenerationBeforeReleaseFailureOrDriftNeverStartsTurn(t *testing.T) {
	for _, failure := range []string{"write failure", "source drift", "canceled callback", "canceled final validation"} {
		t.Run(failure, func(t *testing.T) {
			s, trace := startFixture(t, "generation")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			thread, err := s.StartThread(ctx, ThreadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			blockedPath := filepath.Join(t.TempDir(), "PRIVATE_STORAGE_PATH")
			if err = os.WriteFile(blockedPath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var validations, writes atomic.Int32
			h, err := s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
				return PreparedGeneration{
					History: []HistoryMessage{{Role: "user", Text: "PRIVATE_HISTORY"}},
					Validate: func(context.Context) error {
						if validations.Add(1) == 3 {
							if failure == "source drift" {
								return errors.New("PRIVATE_CHANGED_SOURCE")
							}
							if failure == "canceled final validation" {
								cancel()
							}
						}
						return nil // cancellation must prevent release even with nil
					},
					BeforeRelease: func(context.Context, InjectionReceipt) error {
						writes.Add(1)
						if failure == "write failure" {
							return os.WriteFile(filepath.Join(blockedPath, "receipt"), []byte("PRIVATE_RECEIPT"), 0600)
						}
						if failure == "canceled callback" {
							cancel()
						}
						return nil
					},
					Observe: func(context.Context, GenerationObservation) error { return nil },
				}, nil
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
			_, err = rpc.call(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "PRIVATE_QUESTION"}}})
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("persistence failure leaked or released", err)
			}
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
			defer waitCancel()
			if _, err = h.WaitGeneration(waitCtx); err == nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE") {
				t.Fatal("private failure escaped observation", err)
			}
			_ = h.Close()
			methods, err := os.ReadFile(trace)
			if err != nil || strings.Count(string(methods), "thread/inject_items") != 1 || strings.Contains(string(methods), "turn/start") || writes.Load() != 1 {
				t.Fatal("failed persistence gate released or retried", err)
			}
			expectedValidations := int32(3)
			if failure == "write failure" || failure == "canceled callback" {
				expectedValidations = 2
			}
			if validations.Load() != expectedValidations {
				t.Fatal("post-persistence validation order changed")
			}
		})
	}
}

func TestGenerationBeforeReleaseDisconnectCancelsBlockedWrite(t *testing.T) {
	s, trace := startFixture(t, "generation")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thread, err := s.StartThread(ctx, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	h, err := s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
		return PreparedGeneration{
			History:  []HistoryMessage{{Role: "user", Text: "private history"}},
			Validate: func(context.Context) error { return nil },
			BeforeRelease: func(ctx context.Context, _ InjectionReceipt) error {
				writes.Add(1)
				close(entered)
				<-ctx.Done()
				close(canceled)
				return nil // late success after disconnect still cannot release
			},
			Observe: func(context.Context, GenerationObservation) error { return nil },
		}, nil
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
		t.Fatal("persistence not entered")
	}
	_ = c.CloseNow()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client close did not cancel persistence")
	}
	_ = h.Close()
	methods, err := os.ReadFile(trace)
	if err != nil || writes.Load() != 1 || strings.Count(string(methods), "thread/inject_items") != 1 || strings.Contains(string(methods), "turn/start") {
		t.Fatal("disconnected persistence gate released or retried", err)
	}
}

func TestPreparedGenerationBeforeReleaseStaysPrivate(t *testing.T) {
	p := PreparedGeneration{
		History:       []HistoryMessage{{Role: "user", Text: "PRIVATE_HISTORY"}},
		BeforeRelease: func(context.Context, InjectionReceipt) error { return errors.New("PRIVATE_STORAGE_ERROR") },
	}
	field, exists := reflect.TypeOf(p).FieldByName("BeforeRelease")
	if !exists || field.Tag.Get("json") != "-" {
		t.Fatal("persistence callback is not private")
	}
	raw, err := json.Marshal(p)
	if err != nil || string(raw) != "{}" || strings.Contains(fmt.Sprintf("%v %+v %#v", p, p, p), "PRIVATE") {
		t.Fatal("prepared persistence leaked", err)
	}
}
