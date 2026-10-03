//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func handoffFixture(t *testing.T) (*Session, *Handoff, InjectionReceipt, string) {
	t.Helper()
	s, trace := startFixture(t, "")
	ctx := context.Background()
	if _, err := s.OpenHandoff(ctx); !errors.Is(err, ErrState) {
		t.Fatal("handoff before injection", err)
	}
	if _, err := s.StartThread(ctx, ThreadOptions{}); err != nil {
		t.Fatal(err)
	}
	injection, err := s.InjectHistory(ctx, []HistoryMessage{{"user", "PRIVATE_SYNTHETIC_HISTORY"}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.OpenHandoff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := s.OpenHandoff(ctx); !errors.Is(err, ErrState) {
		t.Fatal("second handoff", err)
	}
	return s, h, injection, trace
}

func dialHandoff(t *testing.T, h *Handoff) (*websocket.Conn, error) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(h.URL(), "unix://"))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, "ws://localhost/rpc", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil && response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, err
}

func initializeHandoff(t *testing.T, c *websocket.Conn) *rpcClient {
	t.Helper()
	rpc := newRPC(c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rpc.call(ctx, "initialize", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := rpc.notify(ctx, "initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	return rpc
}

func TestHandoffSameConnectionReceiptAndNoGeneration(t *testing.T) {
	s, h, injection, trace := handoffFixture(t)
	info, err := os.Stat(strings.TrimPrefix(h.URL(), "unix://"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket privacy", err)
	}
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = rpc.call(ctx, "thread/loaded/list", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.ready:
		t.Fatal("loaded thread is not a TUI resume ACK")
	default:
	}
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": injection.ThreadID, "config": map[string]string{"web_search": "disabled"}, "excludeTurns": true}); err != nil {
		t.Fatal(err)
	}
	r, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ResumeAcknowledged || r.ThreadID != injection.ThreadID || r.PayloadHash != injection.PayloadHash || r.SettingsHash != s.thread.SettingsHash || r.ProviderAcceptance != "unverified" {
		t.Fatalf("bad receipt: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("history leaked into receipt")
	}
	if _, err = dialHandoff(t, h); err == nil {
		t.Fatal("second client accepted")
	}
	if _, err = rpc.call(ctx, "turn/start", map[string]any{"threadId": injection.ThreadID, "input": []any{}}); err == nil {
		t.Fatal("generation permitted")
	}
	if _, err = h.Wait(ctx); err == nil {
		t.Fatal("disconnected receipt reused")
	}
	methods, _ := os.ReadFile(trace)
	if strings.Contains(string(methods), "turn/start") {
		t.Fatal("generation reached native server")
	}
	// Handoff cancellation must not destroy the owned data/control session.
	if _, err = s.rpc.call(ctx, "thread/loaded/list", map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffRejectsWrongThreadBeforeForwarding(t *testing.T) {
	_, h, _, trace := handoffFixture(t)
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": "PRIVATE_OTHER_THREAD"}); err == nil {
		t.Fatal("other thread accepted")
	}
	if _, err = h.Wait(ctx); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal(err)
	}
	methods, _ := os.ReadFile(trace)
	if strings.Contains(string(methods), "thread/resume") {
		t.Fatal("other thread reached server")
	}
}

func TestHandoffCancellationAndSessionClose(t *testing.T) {
	for _, mode := range []string{"waiting", "connected", "session"} {
		t.Run(mode, func(t *testing.T) {
			s, h, _, _ := handoffFixture(t)
			if mode != "waiting" {
				if _, err := dialHandoff(t, h); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if mode == "session" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Wait(ctx); err == nil {
				t.Fatal("unattached connection accepted")
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimPrefix(h.URL(), "unix://")); !os.IsNotExist(err) {
				t.Fatal("handoff socket retained")
			}
		})
	}
}

func TestHandoffStartupDeadlineAndAckHaveOneWinner(t *testing.T) {
	for _, ackFirst := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		h := &Handoff{ctx: ctx, cancel: cancel, ready: make(chan struct{}), startup: time.NewTimer(time.Hour)}
		receipt := HandoffReceipt{ThreadID: "fresh", ResumeAcknowledged: true}
		if ackFirst {
			h.completeResume(receipt)
			// Represents an already-dispatched AfterFunc callback which acquires
			// the mutex after the acknowledgement, despite Timer.Stop.
			h.expireStartup()
			if ctx.Err() != nil || h.err != nil {
				t.Fatal("late startup callback canceled an acknowledged connection")
			}
			select {
			case <-h.ready:
			default:
				t.Fatal("matching ACK did not win")
			}
		} else {
			h.expireStartup()
			h.completeResume(receipt)
			if !errors.Is(h.err, context.DeadlineExceeded) || h.receipt.ResumeAcknowledged {
				t.Fatal("late ACK overrode the deadline")
			}
			select {
			case <-h.ready:
				t.Fatal("late ACK published readiness")
			default:
			}
		}
		h.startup.Stop()
		cancel()
	}
}

func TestHandoffCanceledWaitClosesAlreadyReadyConnection(t *testing.T) {
	_, h, injection, _ := handoffFixture(t)
	c, err := dialHandoff(t, h)
	if err != nil {
		t.Fatal(err)
	}
	rpc := initializeHandoff(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = rpc.call(ctx, "thread/resume", map[string]any{"threadId": injection.ThreadID}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// Both select cases are ready. Either choice must close the bridge.
	stop, stopCancel := context.WithCancel(context.Background())
	stopCancel()
	if _, err = h.Wait(stop); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-h.done:
	default:
		t.Fatal("canceled Wait retained a live bridge")
	}
	if _, err = h.Wait(ctx); err == nil {
		t.Fatal("canceled connection returned a reusable receipt")
	}
}
