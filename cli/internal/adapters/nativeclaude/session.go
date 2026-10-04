// Package nativeclaude owns a versioned, local Claude stream-JSON process.
// It exposes no querying user message, arbitrary control, or permission grant.
// Local context estimates are not provider capacity or acceptance evidence.
package nativeclaude

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrState    = errors.New("native Claude operation is unavailable")
	ErrProtocol = errors.New("native Claude protocol evidence is invalid")
	ErrClosed   = errors.New("native Claude session is closed")
	ErrLimit    = errors.New("native Claude transport limit exceeded")
	ErrCleanup  = errors.New("native Claude process cleanup was not confirmed")
)

const (
	MaxReferenceBytes = 4 << 20
	maxFrameBytes     = 32 << 20
	operationTimeout  = 15 * time.Second
	// Claude 2.1.285 applies this literal projection to isSynthetic messages.
	// Preserve its provenance instead of labelling machine text as user input.
	nativeReferencePrefix = "[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]\n"
)

// Options preserves the caller's environment and explicitly allowed settings.
// Nil Env inherits the environment once, at Start. This adapter neither edits
// configuration nor substitutes credentials. Executable may be a caller-owned
// isolation wrapper which forwards --version and the fixed protocol arguments.
type Options struct {
	Executable string
	Cwd        string
	Env        []string
	Model      string
	ConfigArgs []string
}

type ModelInfo struct {
	Value         string `json:"value"`
	ResolvedModel string `json:"resolved_model,omitempty"`
}

// ContextSummary is the active process's local estimate, including its native
// compaction policy. Neither max field attests backend entitlement. Null API
// usage is required; no prior model usage is accepted by this no-turn adapter.
type ContextSummary struct {
	SessionID            string `json:"session_id"`
	Model                string `json:"model"`
	TotalTokens          int64  `json:"total_tokens"`
	MaxTokens            int64  `json:"max_tokens"`
	RawMaxTokens         int64  `json:"raw_max_tokens"`
	AutoCompactEnabled   bool   `json:"auto_compact_enabled"`
	AutoCompactThreshold *int64 `json:"auto_compact_threshold,omitempty"`
	Measurement          string `json:"measurement"`
}

// ReferenceReceipt contains no reference text, paths, native errors or account
// data. NoTurnAcknowledged means native completed this command with zero model
// usage; ReplayAcknowledged requires an actual exact content echo. Persisted
// means exact post-close readback, not fsync or crash durability. None establish
// provider acceptance.
type ReferenceReceipt struct {
	SessionID          string `json:"session_id"`
	MessageID          string `json:"message_id"`
	PayloadHash        string `json:"payload_hash"`
	UTF8Bytes          int    `json:"utf8_bytes"`
	NativeContentHash  string `json:"native_content_hash"`
	NativeUTF8Bytes    int    `json:"native_utf8_bytes"`
	ReplayAcknowledged bool   `json:"replay_acknowledged"`
	NoTurnAcknowledged bool   `json:"no_turn_acknowledged"`
	Persisted          bool   `json:"persisted"`
	ProviderAcceptance string `json:"provider_acceptance"`
}

type pendingCall struct {
	id        string
	kind      string
	result    chan json.RawMessage
	delivered bool
}

type Session struct {
	id, version, cwd, archiveRoot string
	models                        []ModelInfo
	process                       *process
	gate                          chan struct{}
	mu                            sync.Mutex
	pending                       *pendingCall
	err                           error
	closing                       bool
	appended                      bool
	appendPhase                   int
	appendResult                  bool
	receipt                       ReferenceReceipt
	failed                        chan struct{}
	closed                        chan struct{}
	closeOnce                     sync.Once
	closeErr                      error
}

func (s *Session) SessionID() string   { return s.id }
func (s *Session) HostVersion() string { return s.version }
func (s *Session) Models() []ModelInfo { return append([]ModelInfo(nil), s.models...) }

// Start initializes a fresh native session with no prompt. The context owns the
// lifetime, not just startup: cancellation retires the process and invalidates
// future operations. Protocol support is pinned to 2.1.285.
func Start(ctx context.Context, opts Options) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Freeze symlink resolution before --version. A launcher symlink changing
	// afterward cannot select a different executable for the protocol process.
	executable, err := exec.LookPath(opts.Executable)
	if err != nil {
		return nil, ErrState
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, ErrState
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, ErrState
	}
	opts.Executable = executable
	args, env, cwd, root, err := launchOptions(opts)
	if err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	version, err := readVersion(startup, opts.Executable, cwd, env)
	if err != nil {
		return nil, err
	}
	id, err := newUUID()
	if err != nil {
		return nil, ErrState
	}
	args = append(args, "--session-id", id)
	s := &Session{id: id, version: version, cwd: cwd, archiveRoot: root, gate: make(chan struct{}, 1), failed: make(chan struct{}), closed: make(chan struct{})}
	p, err := startProcess(opts.Executable, args, cwd, env, s.frame, s.fail)
	if err != nil {
		return nil, err
	}
	s.process = p
	go func() {
		select {
		case <-ctx.Done():
			s.fail(ctx.Err())
		case <-s.failed:
		case <-p.outDone:
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return
			}
			s.fail(ErrClosed)
		case <-s.closed:
			return
		}
		_ = s.Close()
	}()
	if err = s.enter(startup); err != nil {
		s.fail(err)
		return nil, errors.Join(err, s.Close())
	}
	raw, err := s.call(startup, "initialize", map[string]any{"subtype": "initialize", "hooks": map[string]any{}, "sdkMcpServers": []any{}, "promptSuggestions": false, "agentProgressSummaries": false})
	if err == nil {
		s.models, err = parseModels(raw)
	}
	<-s.gate
	if err != nil {
		s.fail(err)
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func (s *Session) enter(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.failed:
		return s.failure()
	case <-s.closed:
		return ErrClosed
	case s.gate <- struct{}{}:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.err != nil || s.closing {
		<-s.gate
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.err != nil {
			return s.err
		}
		return ErrClosed
	}
	return nil
}

func (s *Session) ContextSummary(ctx context.Context) (ContextSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := s.enter(ctx); err != nil {
		return ContextSummary{}, err
	}
	defer func() { <-s.gate }()
	raw, err := s.call(ctx, "get_context_usage", map[string]any{"subtype": "get_context_usage", "detail": "summary"})
	if err != nil {
		return ContextSummary{}, err
	}
	value, err := parseSummary(raw, s.id)
	if err != nil {
		s.fail(err)
		_ = s.Close()
	}
	return value, err
}

// AppendReference submits exactly one user-content reference, without querying.
// Text is delivered literally: no slash dispatch or @path expansion. This is
// not an assistant/tool-role history import or a first-question release API.
func (s *Session) AppendReference(ctx context.Context, text string) (ReferenceReceipt, error) {
	if text == "" || !utf8.ValidString(text) {
		return ReferenceReceipt{}, ErrState
	}
	if len(text) > MaxReferenceBytes {
		return ReferenceReceipt{}, ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := s.enter(ctx); err != nil {
		return ReferenceReceipt{}, err
	}
	defer func() { <-s.gate }()
	id, err := newUUID()
	if err != nil {
		return ReferenceReceipt{}, ErrState
	}
	s.mu.Lock()
	if s.appended {
		s.mu.Unlock()
		return ReferenceReceipt{}, ErrState
	}
	s.appended = true
	s.receipt = ReferenceReceipt{SessionID: s.id, MessageID: id, PayloadHash: hashText(text), UTF8Bytes: len(text), NativeContentHash: hashText(nativeReferencePrefix + text), NativeUTF8Bytes: len(nativeReferencePrefix) + len(text), ProviderAcceptance: "unverified"}
	s.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"type": "user", "uuid": id, "session_id": s.id, "parent_tool_use_id": nil, "shouldQuery": false, "isSynthetic": true, "client_composed": true, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
	if err != nil {
		return ReferenceReceipt{}, ErrState
	}
	_, err = s.exchange(ctx, &pendingCall{id: id, kind: "append", result: make(chan json.RawMessage, 1)}, raw)
	if err != nil {
		return ReferenceReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.closing {
		return ReferenceReceipt{}, ErrClosed
	}
	return s.receipt, nil
}

func (s *Session) call(ctx context.Context, kind string, fields map[string]any) (json.RawMessage, error) {
	id, err := newUUID()
	if err != nil {
		return nil, ErrState
	}
	raw, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": fields})
	return s.exchange(ctx, &pendingCall{id: id, kind: kind, result: make(chan json.RawMessage, 1)}, raw)
}

func (s *Session) exchange(ctx context.Context, p *pendingCall, raw []byte) (json.RawMessage, error) {
	s.mu.Lock()
	if s.err != nil || s.closing {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.pending = p
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.pending = nil; s.mu.Unlock() }()
	if err := s.process.write(ctx, append(raw, '\n')); err != nil {
		s.fail(err)
		return nil, errors.Join(err, s.Close())
	}
	select {
	case <-ctx.Done():
		s.fail(ctx.Err())
		return nil, errors.Join(ctx.Err(), s.Close())
	case <-s.failed:
		return nil, errors.Join(s.failure(), s.Close())
	case <-s.closed:
		return nil, ErrClosed
	case result := <-p.result:
		if ctx.Err() != nil {
			s.fail(ctx.Err())
			return nil, errors.Join(ctx.Err(), s.Close())
		}
		if err := s.failure(); err != nil {
			return nil, errors.Join(err, s.Close())
		}
		return result, nil
	}
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		close(s.failed)
	}
}
func (s *Session) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Close drains and validates stdout through EOF, waits for the owned process,
// and never removes its native archive. Forced shutdown is reported as failure,
// not as persisted input. Close is safe concurrently and returns its first result.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		if s.pending != nil && s.err == nil {
			s.err = ErrClosed
			close(s.failed)
		}
		s.mu.Unlock()
		err := s.process.close()
		s.mu.Lock()
		s.closeErr = errors.Join(s.err, err)
		s.mu.Unlock()
		close(s.closed)
	})
	return s.closeErr
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func hashText(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

func launchOptions(o Options) ([]string, []string, string, string, error) {
	if o.Executable == "" || !filepath.IsAbs(o.Cwd) || strings.ContainsRune(o.Model, 0) || len(o.Model) > 256 {
		return nil, nil, "", "", ErrState
	}
	cwd, err := filepath.EvalSymlinks(o.Cwd)
	if err != nil {
		return nil, nil, "", "", ErrState
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, nil, "", "", ErrState
	}
	if !validArgs(o.ConfigArgs) {
		return nil, nil, "", "", ErrState
	}
	env := o.Env
	if env == nil {
		env = os.Environ()
	}
	env = append([]string{}, env...)
	values := map[string]string{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" || strings.ContainsRune(entry, 0) {
			return nil, nil, "", "", ErrState
		}
		if _, ok := values[k]; ok {
			return nil, nil, "", "", ErrState
		}
		values[k] = v
	}
	root := values["CLAUDE_CONFIG_DIR"]
	if root == "" {
		if !filepath.IsAbs(values["HOME"]) {
			return nil, nil, "", "", ErrState
		}
		root = filepath.Join(values["HOME"], ".claude")
	}
	if !filepath.IsAbs(root) {
		return nil, nil, "", "", ErrState
	}
	args := append([]string{}, o.ConfigArgs...)
	args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--await-initialize", "--permission-prompt-tool", "stdio", "--permission-prompts", "none", "--prompt-suggestions", "false", "--replay-user-messages")
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args, env, cwd, filepath.Join(filepath.Clean(root), "projects"), nil
}

func validArgs(args []string) bool {
	if len(args) > 128 {
		return false
	}
	flags := map[string]bool{"--bare": true, "--safe-mode": true, "--strict-mcp-config": true, "--no-chrome": true, "--disable-slash-commands": true}
	values := map[string]bool{"--settings": true, "--setting-sources": true, "--mcp-config": true, "--tools": true, "--allowedTools": true, "--disallowedTools": true, "--add-dir": true, "--plugin-dir": true, "--agent": true, "--system-prompt": true, "--append-system-prompt": true, "--system-prompt-file": true, "--append-system-prompt-file": true, "--effort": true, "--permission-mode": true}
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		if flags[name] && !equal {
			continue
		}
		if !values[name] {
			return false
		}
		if !equal {
			i++
			if i >= len(args) {
				return false
			}
			value = args[i]
		}
		if len(value) > 64<<10 || strings.ContainsRune(value, 0) {
			return false
		}
		if name == "--permission-mode" && value != "default" && value != "dontAsk" && value != "plan" {
			return false
		}
	}
	return true
}
