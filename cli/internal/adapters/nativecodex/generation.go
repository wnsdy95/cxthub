package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Feedback storage is separate from the model turn's outcome. A cache outage
// must be reported without terminating a productive native conversation.
var ErrCalibrationPersistence = errors.New("native Codex input feedback was not confirmed saved")

// Keep observing the client's close while the first request waits for cloud
// preparation. The bounded queue carries bytes only; all protocol decisions
// still run serially in the relay. Overflow cancels rather than hiding a close
// behind an unbounded queue. stop joins the reader on every relay exit.
func (h *Handoff) readGenerationClient(conn *websocket.Conn) (func() (websocket.MessageType, []byte, error), func()) {
	type frame struct {
		kind websocket.MessageType
		data []byte
	}
	ctx, cancel := context.WithCancel(h.ctx)
	queue := make(chan frame, 8)
	done := make(chan struct{})
	var queuedBytes atomic.Int64
	go func() {
		defer close(done)
		defer close(queue)
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					h.finish(ErrClosed)
				}
				return
			}
			if kind != websocket.MessageText || len(data) > maxMessageBytes || queuedBytes.Add(int64(len(data))) > maxNotificationBytes {
				h.finish(handoffError("client queue limit"))
				return
			}
			select {
			case queue <- frame{kind, data}:
			default:
				h.finish(handoffError("client queue limit"))
				return
			}
		}
	}()
	read := func() (websocket.MessageType, []byte, error) {
		select {
		case <-ctx.Done():
			return 0, nil, ErrClosed
		case f, ok := <-queue:
			if !ok || ctx.Err() != nil {
				return 0, nil, ErrClosed
			}
			queuedBytes.Add(-int64(len(f.data)))
			return f.kind, f.data, nil
		}
	}
	return read, func() { cancel(); <-done }
}

// Caller holds the Session gate. Native fresh threads are lazy-persisted; the
// TUI looks them up by ID before resume. Moving this owned, unsectioned thread
// to its existing null section materializes metadata without inventing a user
// message, consuming InjectHistory, or starting a model request (0.157.1).
func (s *Session) materializeFreshThread(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	fail := func() error {
		s.uncertain = true
		_ = s.rpc.close()
		return fmt.Errorf("%w: fresh thread persistence unavailable", ErrState)
	}
	raw, err := s.rpc.call(ctx, "thread/section/move", map[string]any{"threadId": s.thread.ID, "sectionId": nil})
	ack, ok := rpcObject(raw)
	if err != nil || !ok || len(ack) != 0 {
		return fail()
	}
	raw, err = s.rpc.call(ctx, "thread/read", map[string]any{"threadId": s.thread.ID, "includeTurns": true})
	result, ok := identityObject(raw, "thread")
	thread, valid := identityObject(result["thread"], "id", "cwd", "turns")
	var turns []json.RawMessage
	if err != nil || !ok || !valid || !stringEquals(thread["id"], s.thread.ID) || !stringEquals(thread["cwd"], s.thread.Cwd) ||
		json.Unmarshal(thread["turns"], &turns) != nil || turns == nil || len(turns) != 0 {
		return fail()
	}
	return nil
}

func (h *Handoff) prepareGeneration(p *handoffProtocol, s *Session) error {
	p.mu.Lock()
	g := p.generation
	if g == nil || g.firstKey == "" || g.gateTaken {
		p.mu.Unlock()
		return nil
	}
	g.gateTaken = true
	prompt := g.firstPrompt
	g.firstPrompt = ""
	thread := g.thread
	p.mu.Unlock()
	// Preparation may involve cloud reads. No model request is forwarded while
	// this runs; cancellation closes the private connection and owned lifecycle.
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Minute)
	defer cancel()
	prepared, err := h.prepare(ctx, thread, prompt)
	if err != nil || prepared.Validate == nil || prepared.Observe == nil || len(prepared.History) == 0 {
		return fmt.Errorf("%w: initial context preparation failed", ErrState)
	}
	if err = prepared.Validate(ctx); err != nil {
		return fmt.Errorf("%w: initial context changed before injection", ErrState)
	}
	injection, err := s.InjectHistory(ctx, prepared.History)
	if err != nil {
		return fmt.Errorf("%w: initial context injection failed", ErrState)
	}
	// Discard a stale prepared context without ever submitting a model request.
	if err = prepared.Validate(ctx); err != nil || ctx.Err() != nil {
		return fmt.Errorf("%w: initial context changed before generation", ErrState)
	}
	prepared.History = nil
	h.mu.Lock()
	if h.err != nil || h.ctx.Err() != nil {
		h.mu.Unlock()
		return ErrClosed
	}
	h.prepared = &prepared
	h.receipt.PayloadHash = injection.PayloadHash
	h.mu.Unlock()
	p.mu.Lock()
	g.released = true
	p.mu.Unlock()
	return nil
}

func (h *Handoff) recordGeneration(p *handoffProtocol) error {
	p.mu.Lock()
	if p.generation == nil || p.generation.completed == nil {
		p.mu.Unlock()
		return nil
	}
	result := *p.generation.completed
	p.generation.completed = nil
	p.mu.Unlock()
	h.mu.Lock()
	prepared := h.prepared
	h.mu.Unlock()
	if prepared == nil {
		return ErrState
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	var recordingErr error
	if err := prepared.Observe(ctx, result); err != nil {
		var persistence interface{ CalibrationPersistenceFailure() bool }
		if !errors.As(err, &persistence) || !persistence.CalibrationPersistenceFailure() {
			return fmt.Errorf("%w: initial input observation could not be recorded", ErrState)
		}
		recordingErr = ErrCalibrationPersistence
	}
	h.mu.Lock()
	h.generationResult = result
	h.generationErr = recordingErr
	close(h.generationReady)
	h.mu.Unlock()
	return nil
}

// WaitGeneration returns a correlated first-turn observation, not a promise
// that all history was consumed or that the model's answer is correct.
func (h *Handoff) WaitGeneration(ctx context.Context) (GenerationObservation, error) {
	if h.prepare == nil {
		return GenerationObservation{}, ErrState
	}
	select {
	case <-h.generationReady:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.generationResult, h.generationErr
	case <-ctx.Done():
		return GenerationObservation{}, ctx.Err()
	case <-h.done:
		select {
		case <-h.generationReady:
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.generationResult, h.generationErr
		default:
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.err != nil {
			return GenerationObservation{}, h.err
		}
		return GenerationObservation{}, ErrClosed
	}
}
