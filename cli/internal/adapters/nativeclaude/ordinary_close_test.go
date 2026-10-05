//go:build darwin || linux

package nativeclaude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// No child or native IO is needed: process retirement has already completed,
// while writeMu represents a committed permission writer's remaining audit.
func ordinaryCloseSession() *Session {
	p := &process{exited: make(chan struct{}), outDone: make(chan struct{}), errDone: make(chan struct{})}
	close(p.exited)
	close(p.outDone)
	close(p.errDone)
	p.closeOnce.Do(func() {})
	return &Session{process: p, gate: make(chan struct{}, 1), failed: make(chan struct{}), closed: make(chan struct{}),
		firstQuestion: &firstQuestionState{completed: true, ordinary: newOrdinaryState(context.Background(), InteractionHandlers{})}}
}

func awaitOrdinaryClosing(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("Close did not reach its closing transition")
}

func TestOrdinaryCloseDrainsSuccessfulWriterBeforeCancellation(t *testing.T) {
	s := ordinaryCloseSession()
	o := s.firstQuestion.ordinary
	defer o.cancel()
	s.writeMu.Lock()
	locked := true
	done := make(chan struct{})
	var closeErr error
	go func() { closeErr = s.Close(); close(done) }()
	defer func() {
		if locked {
			s.writeMu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Close did not join after releasing the writer")
		}
	}()
	awaitOrdinaryClosing(t, s)
	if err := o.ctx.Err(); err != nil {
		t.Fatalf("successful close canceled the committed writer before its audit: %v", err)
	}
	select {
	case <-done:
		t.Fatal("Close completed before the committed writer audit")
	default:
	}
	s.writeMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("successful Close did not complete")
	}
	if closeErr != nil {
		t.Fatalf("successful writer drain failed: %v", closeErr)
	}
	if !errors.Is(o.ctx.Err(), context.Canceled) {
		t.Fatal("completed Close left the ordinary context live")
	}
}

func TestOrdinaryCloseCancelsFailedWriterBeforeDrain(t *testing.T) {
	for _, cause := range []error{ErrProtocol, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := ordinaryCloseSession()
			o := s.firstQuestion.ordinary
			defer o.cancel()
			s.firstQuestion.completed = false
			s.fail(cause)
			s.writeMu.Lock()
			locked := true
			done := make(chan struct{})
			var closeErr error
			go func() { closeErr = s.Close(); close(done) }()
			defer func() {
				if locked {
					s.writeMu.Unlock()
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("failed Close did not join after releasing the writer")
				}
			}()
			select {
			case <-o.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("failure waited for writer drain before canceling its context")
			}
			select {
			case <-done:
				t.Fatal("failed Close skipped the committed writer audit")
			default:
			}
			s.writeMu.Unlock()
			locked = false
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("failed Close did not complete")
			}
			if !errors.Is(closeErr, cause) {
				t.Fatalf("Close lost the original failure: %v", closeErr)
			}
		})
	}
}

func TestOrdinaryClosePreservesLateCommittedWriterFailure(t *testing.T) {
	s := ordinaryCloseSession()
	o := s.firstQuestion.ordinary
	defer o.cancel()
	// Strict completion failed, but physical retirement is confirmed. An
	// abort must select retirementErr while retaining the writer's ErrClosed.
	s.process.closeErr = ErrCleanup
	s.process.retirementErr = nil
	s.writeMu.Lock()
	locked := true
	done := make(chan struct{})
	var closeErr error
	go func() { closeErr = s.Close(); close(done) }()
	defer func() {
		if locked {
			s.writeMu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("late-failed Close did not join after releasing the writer")
		}
	}()
	awaitOrdinaryClosing(t, s)
	if err := o.ctx.Err(); err != nil {
		t.Fatalf("completed exchange canceled before the writer's late audit: %v", err)
	}
	// The native response was complete before the writer detected its error.
	// Publish the audit failure while Close is blocked on that writer.
	s.fail(ErrClosed)
	s.writeMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late-failed Close did not complete")
	}
	if !errors.Is(closeErr, ErrClosed) || errors.Is(closeErr, ErrCleanup) {
		t.Fatalf("Close lost the late failure or used completion cleanup for abort: %v", closeErr)
	}
	if s.closeErr == nil || !errors.Is(o.ctx.Err(), context.Canceled) {
		t.Fatal("late writer failure became eligible completion or left the ordinary context live")
	}
}

func TestRunOrdinaryDrainsBeforeDeferredCancellation(t *testing.T) {
	// Use the real RunOrdinary/reducer with a synthetic pipe peer. No process
	// launches: the final response is deliberately delivered while the peer
	// holds writeMu as an already committed permission writer's audit.
	f := newFirstExchangeFixture(t, "success")
	executable, err := filepath.EvalSymlinks(f.opts.Executable)
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Executable = executable
	_, env, cwd, _, err := launchOptionsForVersion(f.opts, "2.1.287")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := freezeIdleLaunch(f.opts, env, cwd, "2.1.287")
	if err != nil {
		t.Fatal(err)
	}
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	if err := in.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	s := ordinaryCloseSession()
	s.firstQuestion.ordinary.cancel()
	s.firstQuestion = nil
	s.id, s.version, s.launch = "synthetic-close-session", "2.1.287", launch
	s.appendPhase, s.receipt.NoTurnAcknowledged = 3, true
	s.process.stdin = out
	auditHeld, release := make(chan struct{}), make(chan struct{})
	releaseAudit := sync.OnceFunc(func() { close(release) })
	peerDone := make(chan struct{})
	var peerErr error
	go func() {
		defer close(peerDone)
		peerErr = ordinaryClosePeer(s, in, auditHeld, release)
		if peerErr != nil {
			s.fail(peerErr)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	var result FirstExchangeResult
	var runErr error
	go func() {
		result, runErr = (&FirstExchange{s: s}).RunOrdinary(ctx, "synthetic close question", firstExchangeAllow, InteractionHandlers{})
		close(done)
	}()
	defer func() {
		releaseAudit()
		_ = out.Close()
		for _, joined := range []<-chan struct{}{done, peerDone} {
			select {
			case <-joined:
			case <-time.After(4 * time.Second):
				t.Error("synthetic run/peer did not join")
			}
		}
	}()
	select {
	case <-auditHeld:
	case <-peerDone:
		t.Fatalf("synthetic peer failed before writer audit: %v", peerErr)
	case <-ctx.Done():
		t.Fatal("RunOrdinary did not reach the synthetic response")
	}
	awaitOrdinaryClosing(t, s)
	s.mu.Lock()
	o := s.firstQuestion.ordinary
	s.mu.Unlock()
	if err := o.ctx.Err(); err != nil {
		t.Fatalf("RunOrdinary canceled its committed writer before Close drained it: %v", err)
	}
	select {
	case <-done:
		t.Fatal("RunOrdinary returned before the committed writer audit")
	default:
	}
	releaseAudit()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("RunOrdinary did not finish after writer audit")
	}
	<-peerDone
	if peerErr != nil || runErr != nil || !result.Completed || result.Answer != firstExchangeAnswer {
		t.Fatalf("successful synthetic run failed: peer=%v run=%v completed=%t", peerErr, runErr, result.Completed)
	}
	if !errors.Is(o.ctx.Err(), context.Canceled) {
		t.Fatal("RunOrdinary left its ordinary context live after Close")
	}
}

func ordinaryClosePeer(s *Session, input *os.File, auditHeld chan<- struct{}, release <-chan struct{}) error {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		m, err := object(scanner.Bytes())
		if err != nil {
			return err
		}
		kind, err := stringField(m, "type")
		if err != nil {
			return err
		}
		if kind == "control_request" {
			id, err := stringField(m, "request_id")
			if err != nil {
				return err
			}
			summary := map[string]any{"model": "fixture-model", "totalTokens": 40, "maxTokens": 1000000, "rawMaxTokens": 1000000, "isAutoCompactEnabled": true, "autoCompactThreshold": 967000, "apiUsage": nil}
			raw, err := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": summary}})
			if err != nil {
				return err
			}
			if err := s.frame(raw); err != nil {
				return err
			}
			continue
		}
		if kind != "user" {
			return ErrProtocol
		}
		id, err := stringField(m, "uuid")
		if err != nil {
			return err
		}
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		var frameErr error
		firstExchangeQueryFrames("success", s.id, id, m["message"], func(v any) {
			if frameErr != nil {
				return
			}
			raw, err := json.Marshal(v)
			if err == nil {
				err = s.frame(raw)
			}
			frameErr = err
		})
		if frameErr != nil {
			return frameErr
		}
		close(auditHeld)
		<-release
		return nil
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ErrClosed
}
