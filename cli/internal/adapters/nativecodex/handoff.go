package nativecodex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// HandoffReceipt proves that a matching resume response was forwarded on
// the requesting client's private connection. It is not proof of rendered pixels, available model
// capacity, or provider acceptance. Injection bytes remain private.
type HandoffReceipt struct {
	ThreadID           string `json:"thread_id"`
	PayloadHash        string `json:"payload_hash"`
	SettingsHash       string `json:"settings_hash"`
	ResumeAcknowledged bool   `json:"resume_acknowledged"`
	ProviderAcceptance string `json:"provider_acceptance"`
}

// Handoff is a one-client readiness transport, not a general RPC proxy. It
// permits initialization, inspection, and exactly one resume of the injected
// thread. Model turns and all other mutations remain unavailable, even after
// readiness. A future generation gate must independently revalidate capacity,
// the initial question, and the latest authorized main source.
type Handoff struct {
	url     string
	ctx     context.Context
	cancel  context.CancelFunc
	server  *http.Server
	ready   chan struct{}
	done    chan struct{}
	claimed atomic.Bool
	mu      sync.Mutex
	peers   map[*websocket.Conn]struct{}
	receipt HandoffReceipt
	err     error
	startup *time.Timer
}

func (*Handoff) String() string     { return "native Codex handoff (private transport)" }
func (h *Handoff) GoString() string { return h.String() }

// URL is for the owned TUI's --remote argument, never for a shared receipt.
func (h *Handoff) URL() string { return h.url }

// OpenHandoff requires a successful one-shot injection and an explicit native
// web_search setting. Do not guess the effective default or forward arbitrary
// TUI configuration writes into an already prepared runtime.
func (s *Session) OpenHandoff(ctx context.Context) (*Handoff, error) {
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	if err := s.active(); err != nil {
		return nil, err
	}
	if !s.injected || s.handed || s.socketPath == "" {
		return nil, ErrState
	}
	readCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	raw, err := s.rpc.call(readCtx, "config/read", map[string]any{"cwd": s.cwd, "includeLayers": false})
	cancel()
	if err != nil {
		return nil, err
	}
	result, ok := identityObject(raw, "config")
	config, valid := identityObject(result["config"], "web_search")
	var search string
	if !ok || !valid || json.Unmarshal(config["web_search"], &search) != nil ||
		(search != "cached" && search != "live" && search != "disabled" && search != "indexed") {
		return nil, fmt.Errorf("%w: handoff requires an explicit native web search setting", ErrState)
	}
	path := filepath.Join(s.process.dir, "tui.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: private handoff listener unavailable", ErrState)
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("%w: private handoff permissions unavailable", ErrState)
	}
	hctx, hcancel := context.WithCancel(ctx)
	h := &Handoff{url: "unix://" + path, ctx: hctx, cancel: hcancel, ready: make(chan struct{}), done: make(chan struct{}), peers: make(map[*websocket.Conn]struct{})}
	h.startup = time.AfterFunc(startupTimeout, h.expireStartup)
	h.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	h.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Origin") != "" || !h.claimed.CompareAndSwap(false, true) {
			http.Error(w, "handoff unavailable", http.StatusForbidden)
			return
		}
		client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			h.finish(ErrProtocol)
			return
		}
		client.SetReadLimit(maxMessageBytes)
		h.mu.Lock()
		h.peers[client] = struct{}{}
		h.mu.Unlock()
		defer func() { _ = client.CloseNow(); h.mu.Lock(); delete(h.peers, client); h.mu.Unlock() }()
		connectCtx, connectCancel := context.WithTimeout(hctx, startupTimeout)
		remote, err := dialOwnedSocket(connectCtx, s.socketPath, s.process)
		connectCancel()
		if err != nil {
			h.finish(ErrClosed)
			return
		}
		remote.SetReadLimit(maxMessageBytes)
		h.mu.Lock()
		h.peers[remote] = struct{}{}
		h.mu.Unlock()
		defer func() { _ = remote.CloseNow(); h.mu.Lock(); delete(h.peers, remote); h.mu.Unlock() }()
		state := newHandoffProtocol(s.thread, s.settingsRaw, search)
		results := make(chan error, 2)
		go func() { results <- h.relay(client, remote, state, true, s.injection) }()
		go func() { results <- h.relay(remote, client, state, false, s.injection) }()
		h.finish(<-results)
		_ = client.CloseNow()
		_ = remote.CloseNow()
		<-results
	})
	s.handed = true
	go func() { _ = h.server.Serve(listener); h.cancel() }()
	go func() {
		select {
		case <-hctx.Done():
		case <-s.closed:
		case <-s.process.done:
		}
		h.finish(ErrClosed)
		h.startup.Stop()
		_ = h.server.Close()
		h.mu.Lock()
		for peer := range h.peers {
			_ = peer.CloseNow()
		}
		h.mu.Unlock()
		_ = listener.Close()
		_ = os.Remove(path)
		close(h.done)
	}()
	return h, nil
}

func (h *Handoff) relay(from, to *websocket.Conn, state *handoffProtocol, client bool, injection InjectionReceipt) error {
	for {
		kind, data, err := from.Read(h.ctx)
		if err != nil {
			return ErrClosed
		}
		if kind != websocket.MessageText {
			return ErrProtocol
		}
		resumed, err := state.observe(data, client)
		if err != nil {
			return err
		}
		if err = to.Write(h.ctx, kind, data); err != nil {
			return ErrClosed
		}
		if resumed {
			h.completeResume(HandoffReceipt{ThreadID: injection.ThreadID, PayloadHash: injection.PayloadHash,
				SettingsHash: state.thread.SettingsHash, ResumeAcknowledged: true, ProviderAcceptance: "unverified"})
		}
	}
}

// Timer.Stop alone cannot retract a callback already waiting for this mutex.
// Arbitrate timeout and acknowledgement at the same state transition.
func (h *Handoff) expireStartup() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.receipt.ResumeAcknowledged {
		return
	}
	if h.err == nil {
		h.err = context.DeadlineExceeded
	}
	h.cancel()
}

func (h *Handoff) completeResume(receipt HandoffReceipt) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil || h.ctx.Err() != nil || h.receipt.ResumeAcknowledged {
		return
	}
	h.receipt = receipt
	h.startup.Stop()
	close(h.ready)
}

func (h *Handoff) finish(err error) {
	h.mu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.mu.Unlock()
	h.cancel()
}

// Wait observes live readiness. Cancellation closes the handoff; an old receipt
// is not reusable after disconnect. The returned receipt remains historical.
func (h *Handoff) Wait(ctx context.Context) (HandoffReceipt, error) {
	select {
	case <-ctx.Done():
		h.Close()
		return HandoffReceipt{}, ctx.Err()
	case <-h.ctx.Done():
		h.mu.Lock()
		err := h.err
		h.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return HandoffReceipt{}, err
	case <-h.ready:
	}
	h.mu.Lock()
	if ctx.Err() != nil {
		h.mu.Unlock()
		h.Close()
		return HandoffReceipt{}, ctx.Err()
	}
	defer h.mu.Unlock()
	if h.err != nil {
		return HandoffReceipt{}, h.err
	}
	if h.ctx.Err() != nil {
		return HandoffReceipt{}, ErrClosed
	}
	return h.receipt, nil
}

func (h *Handoff) Close() error {
	h.cancel()
	<-h.done
	return nil
}
