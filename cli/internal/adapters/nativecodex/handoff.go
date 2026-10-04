package nativecodex

import (
	"context"
	"encoding/json"
	"errors"
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

// Handoff is a one-client transport, not a general RPC proxy. OpenHandoff only
// permits inspection and one resume. OpenGenerationHandoff additionally gates
// the exact first question on application preparation and source revalidation.
type Handoff struct {
	url              string
	ctx              context.Context
	cancel           context.CancelFunc
	server           *http.Server
	ready            chan struct{}
	done             chan struct{}
	claimed          atomic.Bool
	prepare          func(context.Context, Thread, string) (PreparedGeneration, error)
	prepared         *PreparedGeneration
	generationReady  chan struct{}
	generationResult GenerationObservation
	generationErr    error
	mu               sync.Mutex
	peers            map[*websocket.Conn]struct{}
	receipt          HandoffReceipt
	err              error
	startup          *time.Timer
}

func (*Handoff) String() string     { return "native Codex handoff (private transport)" }
func (h *Handoff) GoString() string { return h.String() }

// URL is for the owned TUI's --remote argument, never for a shared receipt.
func (h *Handoff) URL() string { return h.url }

// OpenHandoff requires a successful one-shot injection and an explicit native
// web_search setting. Do not guess the effective default or forward arbitrary
// TUI configuration writes into an already prepared runtime.
func (s *Session) OpenHandoff(ctx context.Context) (*Handoff, error) {
	return s.openHandoff(ctx, nil)
}

// PreparedGeneration is invocation-private. The application owns exact text
// accounting, active model/window evidence, latest-main authorization and final
// source/config checks. Neither a callback nor an injection ACK proves model
// acceptance. History and callback errors must never enter routine receipts.
type PreparedGeneration struct {
	History  []HistoryMessage                                   `json:"-"`
	Validate func(context.Context) error                        `json:"-"`
	Observe  func(context.Context, GenerationObservation) error `json:"-"`
}

func (PreparedGeneration) String() string     { return "prepared native generation (private input)" }
func (p PreparedGeneration) GoString() string { return p.String() }

// OpenGenerationHandoff defers preparation until the real initial question is
// known. A single injected history is released to one fresh owned thread. The
// inspection-only OpenHandoff keeps its original no-generation contract.
func (s *Session) OpenGenerationHandoff(ctx context.Context, prepare func(context.Context, Thread, string) (PreparedGeneration, error)) (*Handoff, error) {
	if prepare == nil {
		return nil, ErrState
	}
	return s.openHandoff(ctx, prepare)
}

func (s *Session) openHandoff(ctx context.Context, prepare func(context.Context, Thread, string) (PreparedGeneration, error)) (*Handoff, error) {
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.gate }()
	if err := s.active(); err != nil {
		return nil, err
	}
	if !s.started || s.handed || s.socketPath == "" || (prepare == nil && !s.injected) || (prepare != nil && (s.injected || s.ephemeral)) {
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
	if prepare != nil {
		if err := s.materializeFreshThread(ctx); err != nil {
			return nil, err
		}
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
	h.prepare = prepare
	h.generationReady = make(chan struct{})
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
		if prepare != nil {
			state.generation = newGenerationProtocol(s.thread)
		}
		results := make(chan error, 2)
		injection := s.injection
		go func() { results <- h.relay(client, remote, state, true, injection, s) }()
		go func() { results <- h.relay(remote, client, state, false, injection, s) }()
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

func (h *Handoff) relay(from, to *websocket.Conn, state *handoffProtocol, client bool, injection InjectionReceipt, session *Session) error {
	read := func() (websocket.MessageType, []byte, error) { return from.Read(h.ctx) }
	if client && h.prepare != nil {
		var stop func()
		read, stop = h.readGenerationClient(from)
		defer stop()
	}
	for {
		kind, data, err := read()
		if err != nil {
			return ErrClosed
		}
		if kind != websocket.MessageText {
			return ErrProtocol
		}
		resumed, err := state.observe(data, client)
		if err != nil {
			var denied *deniedAuxiliaryRequest
			if client && errors.As(err, &denied) {
				response, _ := json.Marshal(map[string]any{"id": denied.id, "error": map[string]any{
					"code": -32601, "message": "Auxiliary title generation is unavailable on this managed connection.",
				}})
				if err := from.Write(h.ctx, websocket.MessageText, response); err != nil {
					return ErrClosed
				}
				continue
			}
			return err
		}
		if client {
			if err = h.prepareGeneration(state, session); err != nil {
				return err
			}
		}
		if err = to.Write(h.ctx, kind, data); err != nil {
			return ErrClosed
		}
		if resumed {
			h.completeResume(HandoffReceipt{ThreadID: state.thread.ID, PayloadHash: injection.PayloadHash,
				SettingsHash: state.thread.SettingsHash, ResumeAcknowledged: true, ProviderAcceptance: "unverified"})
		}
		if !client {
			if err = h.recordGeneration(state); err != nil {
				return err
			}
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
	// The client can submit its first question immediately after receiving the
	// resume response. Preparation may already have stored its injection hash.
	if receipt.PayloadHash == "" {
		receipt.PayloadHash = h.receipt.PayloadHash
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
