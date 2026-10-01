package nativecodex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	maxMessageBytes      = 16 << 20
	maxNotifications     = 256
	maxNotificationBytes = 32 << 20
)

var (
	ErrProtocol = errors.New("native codex RPC protocol error")
	ErrClosed   = errors.New("native codex RPC transport closed")
)

// rpcClient owns conn. The gate serializes entire calls, including their reads,
// and notifications. Closing is independent of that gate so it can interrupt IO.
type rpcClient struct {
	conn      *websocket.Conn
	gate      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	nextID    uint64 // protected by gate
}

func newRPC(conn *websocket.Conn) *rpcClient {
	conn.SetReadLimit(maxMessageBytes)
	return &rpcClient{
		conn:   conn,
		gate:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	if c.nextID == ^uint64(0) {
		return nil, c.protocol("request IDs exhausted")
	}
	c.nextID++
	id := c.nextID
	if err := c.write(ctx, &id, method, params); err != nil {
		return nil, err
	}

	// Notifications are discarded immediately. Both budgets apply to the entire
	// wait for this response, so a peer cannot keep a call alive with a flood.
	notifications, notificationBytes := 0, 0
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, c.transportError(ctx, err)
		}
		if err := c.ready(ctx); err != nil {
			return nil, err
		}
		if typ != websocket.MessageText || len(data) > maxMessageBytes {
			return nil, c.protocol("invalid message type or size")
		}
		envelope, ok := rpcObject(data)
		if !ok {
			return nil, c.protocol("invalid JSON object")
		}
		// Codex omits jsonrpc; accept the standard version when it is present.
		if version, present := envelope["jsonrpc"]; present {
			var v string
			if json.Unmarshal(version, &v) != nil || v != "2.0" {
				return nil, c.protocol("invalid JSON-RPC version")
			}
		}
		responseID, hasID := envelope["id"]
		result, hasResult := envelope["result"]
		remoteError, hasError := envelope["error"]
		if methodJSON, hasMethod := envelope["method"]; hasMethod {
			// Never answer a server request, including tool or authentication
			// approvals. Its presence invalidates this restricted transport.
			if hasID {
				return nil, c.protocol("server-initiated requests are unsupported")
			}
			var notificationMethod string
			if hasResult || hasError || json.Unmarshal(methodJSON, &notificationMethod) != nil || notificationMethod == "" {
				return nil, c.protocol("invalid notification")
			}
			notifications++
			notificationBytes += len(data)
			if notifications > maxNotifications || notificationBytes > maxNotificationBytes {
				return nil, c.protocol("notification budget exceeded")
			}
			continue
		}
		if !hasID || !bytes.Equal(responseID, []byte(strconv.FormatUint(id, 10))) {
			return nil, c.protocol("unexpected response ID")
		}
		if hasResult == hasError {
			return nil, c.protocol("response must contain exactly one of result or error")
		}
		if hasResult {
			// Presence, rather than a non-nil decoded value, distinguishes a
			// result. JSON-RPC permits a result whose JSON value is null.
			return result, nil
		}
		errObject, ok := rpcObject(remoteError)
		if !ok {
			return nil, c.protocol("invalid error object")
		}
		codeJSON, hasCode := errObject["code"]
		messageJSON, hasMessage := errObject["message"]
		var code int64
		if !hasCode || bytes.Equal(codeJSON, []byte("null")) || json.Unmarshal(codeJSON, &code) != nil ||
			!hasMessage || len(messageJSON) == 0 || messageJSON[0] != '"' {
			return nil, c.protocol("invalid error fields")
		}
		// Deliberately exclude message, data, and all other peer-controlled
		// text, including WebSocket close reasons, from returned errors.
		return nil, fmt.Errorf("native codex RPC method %q failed with code %d", method, code)
	}
}

func (c *rpcClient) notify(ctx context.Context, method string, params any) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	return c.write(ctx, nil, method, params)
}

func (c *rpcClient) write(ctx context.Context, id *uint64, method string, params any) error {
	if method == "" {
		return c.protocol("empty method")
	}
	data, err := json.Marshal(struct {
		ID     *uint64 `json:"id,omitempty"`
		Method string  `json:"method"`
		Params any     `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params})
	if err != nil {
		// A custom marshaler's error can itself contain sensitive values.
		return c.protocol("cannot encode outgoing message")
	}
	if len(data) > maxMessageBytes {
		return c.protocol("outgoing message too large")
	}
	if err := c.ready(ctx); err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return c.transportError(ctx, err)
	}
	return nil
}

func (c *rpcClient) acquire(ctx context.Context) error {
	if err := c.ready(ctx); err != nil {
		return err
	}
	select {
	case c.gate <- struct{}{}:
		if err := c.ready(ctx); err != nil {
			c.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return c.stop(ctx.Err())
	case <-c.closed:
		return ErrClosed
	}
}

func (c *rpcClient) release() { <-c.gate }

func (c *rpcClient) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return c.stop(err)
	}
	select {
	case <-c.closed:
		return ErrClosed
	default:
		return nil
	}
}

func (c *rpcClient) protocol(reason string) error {
	return c.stop(fmt.Errorf("%w: %s", ErrProtocol, reason))
}

func (c *rpcClient) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return c.stop(ctx.Err())
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return c.protocol("incoming message too large")
	}
	return c.stop(ErrClosed)
}

func (c *rpcClient) stop(err error) error {
	_ = c.close()
	return err
}

func (c *rpcClient) close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		// No close handshake: it would wait on an untrusted peer. CloseNow
		// unblocks concurrent IO without ever acquiring the request gate.
		_ = c.conn.CloseNow()
	})
	return nil
}

// rpcObject rejects non-objects, trailing JSON, invalid UTF-8, and duplicate
// fields rather than allowing ambiguous IDs or result/error keys to be hidden.
func rpcObject(data []byte) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, duplicate := object[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		object[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return object, true
}
