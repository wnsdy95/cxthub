//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHandoffExitRequiresOwnedResumedThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  int // 0: new, 1: initialized, 2: resume pending, 3: resumed
		params string
	}{
		{"before initialize", 0, `{"threadId":"prepared-thread"}`},
		{"before resume", 1, `{"threadId":"prepared-thread"}`},
		{"before resume acknowledgement", 2, `{"threadId":"prepared-thread"}`},
		{"foreign thread", 3, `{"threadId":"other-thread"}`},
		{"missing thread", 3, `{}`},
		{"null thread", 3, `{"threadId":null}`},
		{"numeric thread", 3, `{"threadId":1}`},
		{"extra parameter", 3, `{"threadId":"prepared-thread","force":false}`},
		{"aliased identity", 3, `{"threadId":"prepared-thread","ThreadId":"other-thread"}`},
		{"duplicate identity", 3, `{"threadId":"prepared-thread","threadId":"prepared-thread"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			if tc.stage >= 1 {
				f.initialize(t)
			}
			if tc.stage >= 2 {
				f.resume(t, "resume")
			}
			if tc.stage >= 3 {
				handoffUnitObserve(t, f.p, handoffUnitResponse(t, "resume", f.result), false, true, false)
			}
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, "exit", "thread/unsubscribe", json.RawMessage(tc.params)), true, false, true)
			if !errors.Is(f.p.clientCloseError(), ErrClosed) {
				t.Fatal("invalid unsubscribe granted clean exit")
			}
		})
	}
}

func TestHandoffExitRequiresCorrelatedSuccessfulAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		accept   bool
	}{
		{"unsubscribed", `{"id":17,"result":{"status":"unsubscribed"}}`, true},
		{"not subscribed", `{"id":17,"result":{"status":"notSubscribed"}}`, false},
		{"not loaded", `{"id":17,"result":{"status":"notLoaded"}}`, false},
		{"missing status", `{"id":17,"result":{}}`, false},
		{"null result", `{"id":17,"result":null}`, false},
		{"null status", `{"id":17,"result":{"status":null}}`, false},
		{"wrong status type", `{"id":17,"result":{"status":true}}`, false},
		{"unknown status", `{"id":17,"result":{"status":"closed"}}`, false},
		{"case alias", `{"id":17,"result":{"status":"unsubscribed","Status":"notLoaded"}}`, false},
		{"duplicate status", `{"id":17,"result":{"status":"notLoaded","status":"unsubscribed"}}`, false},
		{"wrong id", `{"id":18,"result":{"status":"unsubscribed"}}`, false},
		{"string id is distinct", `{"id":"17","result":{"status":"unsubscribed"}}`, false},
		{"RPC error", `{"id":17,"error":{"code":-32603,"message":"denied"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffUnitFixture(t)
			f.ready(t)
			handoffUnitObserve(t, f.p, handoffUnitRequest(t, 17, "thread/unsubscribe", map[string]any{"threadId": f.p.thread.ID}), true, false, false)
			if !errors.Is(f.p.clientCloseError(), ErrClosed) {
				t.Fatal("request without acknowledgement granted clean exit")
			}
			handoffUnitObserve(t, f.p, []byte(tc.response), false, false, !tc.accept)
			want := ErrClosed
			if tc.accept {
				want = ErrClientExit
			}
			if !errors.Is(f.p.clientCloseError(), want) {
				t.Fatalf("client closure = %v, want %v", f.p.clientCloseError(), want)
			}
		})
	}
}

func TestHandoffExitInspectionResponseCannotAcknowledgeUnsubscribe(t *testing.T) {
	f := newHandoffUnitFixture(t)
	f.ready(t)
	params := map[string]any{"threadId": f.p.thread.ID}
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "read", "thread/read", params), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "exit", "thread/unsubscribe", params), true, false, false)
	ack := map[string]any{"status": "unsubscribed"}
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "read", ack), false, false, false)
	if !errors.Is(f.p.clientCloseError(), ErrClosed) {
		t.Fatal("unrelated response granted clean exit")
	}
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "exit", ack), false, false, false)
	if !errors.Is(f.p.clientCloseError(), ErrClientExit) {
		t.Fatal("correlated unsubscribe did not grant terminal client close")
	}
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "exit", ack), false, false, true)
}

func TestHandoffExitRejectsFurtherClientRequests(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		for _, method := range []string{"thread/unsubscribe", "thread/resume", "thread/read", "turn/start", "config/read"} {
			name := "pending/" + method
			if acknowledged {
				name = "acknowledged/" + method
			}
			t.Run(name, func(t *testing.T) {
				f := newHandoffUnitFixture(t)
				f.p.generation = newGenerationProtocol(f.p.thread)
				f.ready(t)
				params := map[string]any{"threadId": f.p.thread.ID}
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, "exit", "thread/unsubscribe", params), true, false, false)
				if acknowledged {
					handoffUnitObserve(t, f.p, handoffUnitResponse(t, "exit", map[string]any{"status": "unsubscribed"}), false, false, false)
				}
				if method == "turn/start" {
					params["input"] = []any{map[string]any{"type": "text", "text": "must not run"}}
				}
				// Use a fresh ID so duplicate-ID rejection cannot hide a missing
				// terminal gate. The read and turn would otherwise be permitted.
				handoffUnitObserve(t, f.p, handoffUnitRequest(t, "later", method, params), true, false, true)
				if f.p.generation.active || f.p.generation.firstKey != "" {
					t.Fatal("terminal request opened a generation gate")
				}
			})
		}
	}
}

func TestHandoffExitReadFailureRemainsFatalAfterAcknowledgement(t *testing.T) {
	client, peer, parent := rpcFixture(t, nil)
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	hctx, hcancel := context.WithCancel(ctx)
	defer hcancel()
	h := &Handoff{ctx: hctx, cancel: hcancel}
	f := newHandoffUnitFixture(t)
	f.ready(t)
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "exit", "thread/unsubscribe", map[string]any{"threadId": f.p.thread.ID}), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "exit", map[string]any{"status": "unsubscribed"}), false, false, false)
	// Exercise the real WebSocket read-limit error without allocating a 16 MiB
	// frame. An acknowledged unsubscribe must not turn a protocol failure into
	// the exact nonfatal ErrClientExit sentinel consumed by the monitor.
	peer.SetReadLimit(32)
	_, stop := h.readGenerationClient(peer, f.p)
	defer stop()
	client.conn.CloseRead(ctx)
	_ = client.conn.Write(ctx, websocket.MessageText, []byte(strings.Repeat("x", 64)))
	select {
	case <-h.ctx.Done():
	case <-ctx.Done():
		t.Fatal("read-limit violation did not terminate the handoff")
	}
	h.mu.Lock()
	err := h.err
	h.mu.Unlock()
	if err == nil || err == ErrClientExit || ctx.Err() != nil {
		t.Fatalf("read-limit violation classified as %v; want fatal error (context: %v)", err, ctx.Err())
	}
}

func TestHandoffExitQueuedRequestBeforeCloseRemainsFatal(t *testing.T) {
	client, peer, parent := rpcFixture(t, nil)
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	hctx, hcancel := context.WithCancel(ctx)
	defer hcancel()
	h := &Handoff{ctx: hctx, cancel: hcancel}
	f := newHandoffUnitFixture(t)
	f.ready(t)
	params := map[string]any{"threadId": f.p.thread.ID}
	handoffUnitObserve(t, f.p, handoffUnitRequest(t, "exit", "thread/unsubscribe", params), true, false, false)
	handoffUnitObserve(t, f.p, handoffUnitResponse(t, "exit", map[string]any{"status": "unsubscribed"}), false, false, false)
	_, stop := h.readGenerationClient(peer, f.p)
	defer stop()
	client.conn.CloseRead(ctx)
	// Hold the relay consumer behind the reader. A post-unsubscribe request is
	// already forbidden; a following close must not erase that violation before
	// the consumer gets scheduled to validate the queued frame.
	request := handoffUnitRequest(t, "later", "thread/read", params)
	if err := client.conn.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	// A reader that rejects the terminal request immediately can close before
	// replying to Ping. Otherwise the pong proves it queued the request.
	_ = client.conn.Ping(ctx)
	_ = client.conn.Close(websocket.StatusNormalClosure, "")
	select {
	case <-h.ctx.Done():
	case <-ctx.Done():
		t.Fatal("client close did not terminate the handoff")
	}
	h.mu.Lock()
	err := h.err
	h.mu.Unlock()
	if err == nil || err == ErrClientExit || ctx.Err() != nil {
		t.Fatalf("queued terminal violation classified as %v; want fatal error (context: %v)", err, ctx.Err())
	}
}

func TestHandoffExitTransportDistinguishesTerminalClientAndRuntimeLoss(t *testing.T) {
	for _, generation := range []bool{false, true} {
		mode := "inspection"
		if generation {
			mode = "generation"
		}
		for _, tc := range []struct {
			name        string
			unsubscribe bool
			close       string
			want        error
		}{
			{"acknowledged normal close", true, "normal", ErrClientExit},
			{"acknowledged socket close", true, "abrupt", ErrClientExit},
			{"unannounced normal close", false, "normal", ErrClosed},
			{"unannounced socket close", false, "abrupt", ErrClosed},
			{"owned RPC loss after acknowledgement", true, "rpc", ErrClosed},
			{"owned process loss after acknowledgement", true, "process", ErrClosed},
			{"request after acknowledgement", true, "request", ErrProtocol},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				s, trace := startFixture(t, "")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				thread, err := s.StartThread(ctx, ThreadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				var h *Handoff
				if generation {
					h, err = s.OpenGenerationHandoff(ctx, func(context.Context, Thread, string) (PreparedGeneration, error) {
						t.Error("idle exit invoked generation preparation")
						return PreparedGeneration{}, ErrState
					})
				} else {
					if _, err := s.InjectHistory(ctx, []HistoryMessage{{Role: "user", Text: "fixture history"}}); err != nil {
						t.Fatal(err)
					}
					h, err = s.OpenHandoff(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer h.Close()
				c, err := dialHandoff(t, h)
				if err != nil {
					t.Fatal(err)
				}
				rpc := initializeHandoff(t, c)
				params := map[string]any{"threadId": thread.ID}
				if _, err := rpc.call(ctx, "thread/resume", params); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if tc.unsubscribe {
					ack, err := rpc.call(ctx, "thread/unsubscribe", params)
					if err != nil || !sameJSON(ack, []byte(`{"status":"unsubscribed"}`)) {
						t.Fatalf("unsubscribe acknowledgement = %s, %v", ack, err)
					}
					if err := h.ctx.Err(); err != nil {
						t.Fatalf("acknowledgement ended lifecycle before client closure: %v", err)
					}
				}
				switch tc.close {
				case "normal":
					_ = c.Close(websocket.StatusNormalClosure, "")
				case "abrupt":
					_ = c.CloseNow()
				case "rpc":
					_ = s.rpc.close()
				case "process":
					if err := s.process.stop(); err != nil {
						t.Fatal(err)
					}
				case "request":
					if _, err := rpc.call(ctx, "thread/read", params); err == nil {
						t.Fatal("request after terminal unsubscribe was forwarded")
					}
				}
				if err := h.WaitLifecycle(ctx); !errors.Is(err, tc.want) || ctx.Err() != nil {
					t.Fatalf("lifecycle = %v, want %v (context: %v)", err, tc.want, ctx.Err())
				}
				if generation {
					observation, err := h.WaitGeneration(ctx)
					if !errors.Is(err, tc.want) || observation.ThreadID != "" || observation.ExecutionStarted || observation.Outcome != "" {
						t.Fatalf("idle exit invented acceptance: observation=%+v err=%v", observation, err)
					}
				}
				data, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				unsubscribes := 0
				for _, method := range strings.Fields(string(data)) {
					if method == "turn/start" || generation && method == "thread/inject_items" {
						t.Fatalf("idle exit forwarded %s", method)
					}
					if method == "thread/unsubscribe" {
						unsubscribes++
					}
				}
				if (unsubscribes == 1) != tc.unsubscribe || unsubscribes > 1 {
					t.Fatalf("forwarded %d unsubscribes, requested=%v", unsubscribes, tc.unsubscribe)
				}
			})
		}
	}
}
