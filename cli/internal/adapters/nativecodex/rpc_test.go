package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type rpcOutcome struct {
	result json.RawMessage
	err    error
}

func rpcFixture(t *testing.T, transport *http.Transport) (*rpcClient, *websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxMessageBytes + 1024)
		accepted <- conn
		<-ctx.Done()
	}))
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	var opts *websocket.DialOptions
	if transport != nil {
		opts = &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}}
		t.Cleanup(transport.CloseIdleConnections)
	}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), opts)
	if err != nil {
		t.Fatal(err)
	}
	client := newRPC(conn)
	t.Cleanup(func() { _ = client.close() })
	select {
	case peer := <-accepted:
		return client, peer, ctx
	case <-ctx.Done():
		t.Fatal("server did not accept websocket")
		return nil, nil, nil
	}
}

func rpcStartCall(client *rpcClient, ctx context.Context, method string, params any) <-chan rpcOutcome {
	done := make(chan rpcOutcome, 1)
	go func() {
		result, err := client.call(ctx, method, params)
		done <- rpcOutcome{result: result, err: err}
	}()
	return done
}

func rpcAwait(t *testing.T, done <-chan rpcOutcome) rpcOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("RPC did not finish promptly")
		return rpcOutcome{}
	}
}

func rpcRead(t *testing.T, peer *websocket.Conn, ctx context.Context) map[string]json.RawMessage {
	t.Helper()
	typ, data, err := peer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText {
		t.Fatal("outgoing RPC was not text")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		t.Fatalf("invalid outgoing object: %v", err)
	}
	return object
}

func rpcWrite(t *testing.T, peer *websocket.Conn, ctx context.Context, data string) {
	t.Helper()
	if err := peer.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

func rpcAssertClosed(t *testing.T, client *rpcClient, peer *websocket.Conn, ctx context.Context) {
	t.Helper()
	if _, err := client.call(ctx, "must-not-retry", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after failure = %v, want ErrClosed", err)
	}
	if err := client.notify(ctx, "must-not-notify", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("notify after failure = %v, want ErrClosed", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, _, err := peer.Read(readCtx); err == nil || readCtx.Err() != nil {
		t.Fatalf("peer did not observe immediate closure without another request: %v", err)
	}
}

func TestRPCNotificationInterleavingAndIDs(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	for id := 1; id <= 2; id++ {
		done := rpcStartCall(client, ctx, "thread/read", map[string]any{"threadId": "local"})
		request := rpcRead(t, peer, ctx)
		if string(request["id"]) != strconv.Itoa(id) || string(request["method"]) != `"thread/read"` || string(request["params"]) != `{"threadId":"local"}` {
			t.Fatalf("unexpected request: %s", request)
		}
		for range maxNotifications {
			rpcWrite(t, peer, ctx, `{"method":"thread/status","params":{"private":"discard-me"}}`)
		}
		rpcWrite(t, peer, ctx, fmt.Sprintf(`{"id":%d,"result":{"ok":true}}`, id))
		outcome := rpcAwait(t, done)
		if outcome.err != nil || string(outcome.result) != `{"ok":true}` {
			t.Fatalf("call = %s, %v", outcome.result, outcome.err)
		}
	}
	if err := client.notify(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}
	notification := rpcRead(t, peer, ctx)
	if _, ok := notification["id"]; ok || string(notification["method"]) != `"initialized"` {
		t.Fatalf("unexpected notification: %s", notification)
	}
}

func TestRPCRejectsInvalidEnvelopes(t *testing.T) {
	cases := map[string]string{
		"malformed":             `{"id":1,"result":`,
		"null envelope":         `null`,
		"array envelope":        `[{"id":1,"result":{}}]`,
		"scalar envelope":       `true`,
		"missing id":            `{"result":null}`,
		"wrong id":              `{"id":2,"result":{}}`,
		"old id":                `{"id":0,"result":{}}`,
		"string id":             `{"id":"1","result":{}}`,
		"null id":               `{"id":null,"result":{}}`,
		"fractional id":         `{"id":1.0,"result":{}}`,
		"exponent id":           `{"id":1e0,"result":{}}`,
		"negative id":           `{"id":-1,"result":{}}`,
		"overflow id":           `{"id":18446744073709551616,"result":{}}`,
		"result and error":      `{"id":1,"result":{},"error":{"code":-1,"message":"secret"}}`,
		"result and null error": `{"id":1,"result":{},"error":null}`,
		"neither result error":  `{"id":1}`,
		"null error":            `{"id":1,"error":null}`,
		"string error":          `{"id":1,"error":"secret"}`,
		"missing code":          `{"id":1,"error":{"message":"secret"}}`,
		"null code":             `{"id":1,"error":{"code":null,"message":"secret"}}`,
		"fractional code":       `{"id":1,"error":{"code":1.5,"message":"secret"}}`,
		"null error message":    `{"id":1,"error":{"code":-1,"message":null}}`,
		"invalid version":       `{"jsonrpc":"1.0","id":1,"result":{}}`,
		"trailing object":       `{"id":1,"result":{}} {"id":1,"result":{}}`,
		"duplicate id":          `{"id":2,"id":1,"result":{}}`,
		"escaped duplicate id":  `{"id":2,"i\u0064":1,"result":{}}`,
		"duplicate result":      `{"id":1,"result":null,"result":{}}`,
		"duplicate error code":  `{"id":1,"error":{"code":1,"code":2,"message":"secret"}}`,
		"null method":           `{"method":null}`,
		"empty method":          `{"method":""}`,
		"notification result":   `{"method":"event","result":{}}`,
		"notification error":    `{"method":"event","error":{}}`,
		"invalid UTF8":          "{\"id\":1,\"result\":\"\xff\"}",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			client, peer, ctx := rpcFixture(t, nil)
			done := rpcStartCall(client, ctx, "thread/read", nil)
			rpcRead(t, peer, ctx)
			rpcWrite(t, peer, ctx, payload)
			outcome := rpcAwait(t, done)
			if !errors.Is(outcome.err, ErrProtocol) || outcome.result != nil {
				t.Fatalf("call = %s, %v, want protocol failure", outcome.result, outcome.err)
			}
			if strings.Contains(outcome.err.Error(), "secret") {
				t.Fatal("protocol error leaked payload")
			}
			rpcAssertClosed(t, client, peer, ctx)
		})
	}
}

func TestRPCJSONNullResult(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	done := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	rpcWrite(t, peer, ctx, `{"jsonrpc":"2.0","id":1,"result":null}`)
	outcome := rpcAwait(t, done)
	if outcome.err != nil || string(outcome.result) != "null" {
		t.Fatalf("JSON null result = %s, %v", outcome.result, outcome.err)
	}
}

func TestRPCBinaryMessage(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	done := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	if err := peer.Write(ctx, websocket.MessageBinary, []byte(`{"id":1,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if outcome := rpcAwait(t, done); !errors.Is(outcome.err, ErrProtocol) {
		t.Fatalf("binary message = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCLateResponsePoisonsNextCall(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	first := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	rpcWrite(t, peer, ctx, `{"id":1,"result":{}}`)
	if outcome := rpcAwait(t, first); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	second := rpcStartCall(client, ctx, "thread/read", nil)
	if request := rpcRead(t, peer, ctx); string(request["id"]) != "2" {
		t.Fatalf("next ID = %s", request["id"])
	}
	rpcWrite(t, peer, ctx, `{"id":1,"result":{"late":true}}`)
	if outcome := rpcAwait(t, second); !errors.Is(outcome.err, ErrProtocol) {
		t.Fatalf("late response = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCServerRequestsNeverApproved(t *testing.T) {
	for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "account/authenticate", "item/tool/call"} {
		t.Run(method, func(t *testing.T) {
			client, peer, ctx := rpcFixture(t, nil)
			done := rpcStartCall(client, ctx, "thread/read", nil)
			rpcRead(t, peer, ctx)
			rpcWrite(t, peer, ctx, fmt.Sprintf(`{"id":null,"method":%q,"params":{"secret":"do-not-expose"}}`, method))
			outcome := rpcAwait(t, done)
			if !errors.Is(outcome.err, ErrProtocol) || !strings.Contains(outcome.err.Error(), "server-initiated") {
				t.Fatalf("server request = %v", outcome.err)
			}
			rpcAssertClosed(t, client, peer, ctx)
		})
	}
}

func TestRPCRemoteErrorRedactedAndConnectionReusable(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	done := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	rpcWrite(t, peer, ctx, `{"id":1,"error":{"code":-32001,"message":"secret-token","data":{"password":"secret-password"},"extra":"secret-extra"}}`)
	outcome := rpcAwait(t, done)
	if outcome.err == nil || outcome.err.Error() != `native codex RPC method "thread/read" failed with code -32001` || outcome.result != nil {
		t.Fatalf("unexpected sanitized error: %v", outcome.err)
	}
	done = rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	rpcWrite(t, peer, ctx, `{"id":2,"result":{}}`)
	if outcome := rpcAwait(t, done); outcome.err != nil {
		t.Fatalf("call after remote error: %v", outcome.err)
	}
}

func TestRPCOutgoingBound(t *testing.T) {
	for _, notification := range []bool{false, true} {
		t.Run(fmt.Sprintf("notification=%v", notification), func(t *testing.T) {
			client, peer, ctx := rpcFixture(t, nil)
			// The raw string fits, but its JSON escaping exceeds the limit.
			params := strings.Repeat("\x00", maxMessageBytes/6)
			var err error
			if notification {
				err = client.notify(ctx, "initialized", params)
			} else {
				_, err = client.call(ctx, "thread/read", params)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("oversized outgoing message = %v", err)
			}
			rpcAssertClosed(t, client, peer, ctx)
		})
	}
}

func TestRPCIncomingBound(t *testing.T) {
	for _, over := range []bool{false, true} {
		t.Run(fmt.Sprintf("over=%v", over), func(t *testing.T) {
			client, peer, ctx := rpcFixture(t, nil)
			done := rpcStartCall(client, ctx, "thread/read", nil)
			rpcRead(t, peer, ctx)
			prefix, suffix := `{"id":1,"result":"`, `"}`
			size := maxMessageBytes
			if over {
				size++
			}
			payload := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
			err := peer.Write(ctx, websocket.MessageText, []byte(payload))
			if !over && err != nil {
				t.Fatal(err)
			}
			outcome := rpcAwait(t, done)
			if over {
				if !errors.Is(outcome.err, ErrProtocol) {
					t.Fatalf("oversized incoming message = %v", outcome.err)
				}
				rpcAssertClosed(t, client, peer, ctx)
			} else if outcome.err != nil || len(outcome.result) != size-len(prefix)-len(suffix)+2 {
				t.Fatalf("at-limit incoming result bytes=%d, err=%v", len(outcome.result), outcome.err)
			}
		})
	}
}

func TestRPCNotificationBudgets(t *testing.T) {
	for _, byteBudget := range []bool{false, true} {
		t.Run(fmt.Sprintf("bytes=%v", byteBudget), func(t *testing.T) {
			client, peer, ctx := rpcFixture(t, nil)
			done := rpcStartCall(client, ctx, "thread/read", nil)
			rpcRead(t, peer, ctx)
			payload := `{"method":"event","params":{}}`
			count := maxNotifications + 1
			if byteBudget {
				payload = `{"method":"event","params":{"text":"` + strings.Repeat("x", 1<<20) + `"}}`
				count = maxNotificationBytes/len(payload) + 1
				if count > maxNotifications || len(payload) > maxMessageBytes {
					t.Fatal("fixture must exceed only the cumulative byte budget")
				}
			}
			for i := 0; i < count; i++ {
				if err := peer.Write(ctx, websocket.MessageText, []byte(payload)); err != nil && i != count-1 {
					t.Fatal(err)
				}
			}
			if outcome := rpcAwait(t, done); !errors.Is(outcome.err, ErrProtocol) {
				t.Fatalf("notification flood = %v", outcome.err)
			}
			rpcAssertClosed(t, client, peer, ctx)
		})
	}
}

func TestRPCCancellationPoisonsTransport(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	callCtx, cancel := context.WithCancel(ctx)
	done := rpcStartCall(client, callCtx, "side-effecting-method", nil)
	rpcRead(t, peer, ctx)
	cancel()
	if outcome := rpcAwait(t, done); !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("canceled call = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCDeadlinePoisonsTransport(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	callCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	done := rpcStartCall(client, callCtx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	if outcome := rpcAwait(t, done); !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("timed-out call = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCQueuedCancellationInterruptsActiveCall(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	active := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	queuedCtx, cancel := context.WithCancel(ctx)
	queued := rpcStartCall(client, queuedCtx, "never-sent", nil)
	cancel()
	if outcome := rpcAwait(t, queued); !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", outcome.err)
	}
	if outcome := rpcAwait(t, active); !errors.Is(outcome.err, ErrClosed) {
		t.Fatalf("active call after cancellation = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCCloseInterruptsReadAndQueuedWrites(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	active := rpcStartCall(client, ctx, "thread/read", nil)
	rpcRead(t, peer, ctx)
	const n = 16
	results := make(chan error, n)
	for range n {
		go func() { results <- client.notify(ctx, "initialized", nil) }()
	}
	var closers sync.WaitGroup
	for range n {
		closers.Go(func() {
			if err := client.close(); err != nil {
				t.Errorf("close = %v", err)
			}
		})
	}
	if outcome := rpcAwait(t, active); !errors.Is(outcome.err, ErrClosed) {
		t.Fatalf("interrupted call = %v", outcome.err)
	}
	for range n {
		select {
		case err := <-results:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("queued notify = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("queued notify did not unblock")
		}
	}
	closers.Wait()
	rpcAssertClosed(t, client, peer, ctx)
}

// Inject a blocked socket write after the HTTP upgrade, making close-during-
// write deterministic without relying on sleeps or operating-system buffers.
type rpcBlockedConn struct {
	net.Conn
	block     atomic.Bool
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (c *rpcBlockedConn) Write(data []byte) (int, error) {
	if c.block.Load() {
		c.startOnce.Do(func() { close(c.started) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(data)
}

func (c *rpcBlockedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestRPCCloseInterruptsWrite(t *testing.T) {
	var socket *rpcBlockedConn
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		socket = &rpcBlockedConn{Conn: conn, started: make(chan struct{}), closed: make(chan struct{})}
		return socket, nil
	}}
	client, peer, ctx := rpcFixture(t, transport)
	socket.block.Store(true)
	done := make(chan rpcOutcome, 1)
	go func() { done <- rpcOutcome{err: client.notify(ctx, "initialized", nil)} }()
	select {
	case <-socket.started:
	case <-ctx.Done():
		t.Fatal("write did not start")
	}
	closed := make(chan rpcOutcome, 1)
	go func() { closed <- rpcOutcome{err: client.close()} }()
	if outcome := rpcAwait(t, closed); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome := rpcAwait(t, done); !errors.Is(outcome.err, ErrClosed) {
		t.Fatalf("interrupted write = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCDisconnectRedactsCloseReason(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	done := rpcStartCall(client, ctx, "side-effecting-method", nil)
	rpcRead(t, peer, ctx)
	_ = peer.Close(websocket.StatusPolicyViolation, "sensitive-server-secret")
	outcome := rpcAwait(t, done)
	if !errors.Is(outcome.err, ErrClosed) || strings.Contains(outcome.err.Error(), "sensitive") {
		t.Fatalf("disconnection = %v", outcome.err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}

func TestRPCConcurrentCallsAndNotifications(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	const count = 24
	results := make(chan error, count*2)
	for range count {
		go func() {
			result, err := client.call(ctx, "thread/read", nil)
			if err == nil && string(result) != `{"ok":true}` {
				err = errors.New("incorrect result")
			}
			results <- err
		}()
		go func() { results <- client.notify(ctx, "initialized", nil) }()
	}
	id, notifications := 0, 0
	for range count * 2 {
		request := rpcRead(t, peer, ctx)
		if rawID, present := request["id"]; present {
			id++
			if string(rawID) != strconv.Itoa(id) {
				t.Fatalf("ID = %s, want %d", rawID, id)
			}
			rpcWrite(t, peer, ctx, fmt.Sprintf(`{"id":%d,"result":{"ok":true}}`, id))
		} else {
			notifications++
		}
	}
	if id != count || notifications != count {
		t.Fatalf("received %d requests and %d notifications", id, notifications)
	}
	for range count * 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

type rpcSensitiveMarshaler struct{}

func (rpcSensitiveMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("sensitive-marshaler-secret")
}

func TestRPCMarshalErrorRedacted(t *testing.T) {
	client, peer, ctx := rpcFixture(t, nil)
	_, err := client.call(ctx, "thread/read", rpcSensitiveMarshaler{})
	if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("marshal error = %v", err)
	}
	rpcAssertClosed(t, client, peer, ctx)
}
