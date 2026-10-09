// Package nativeclaude owns a versioned, local Claude stream-JSON process.
// Start retains a no-query reference contract. FirstExchange owns an admitted
// first turn with bounded, explicitly handled native tool interactions.
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

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
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
	// The pinned native protocol applies this literal projection to isSynthetic messages.
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
	// SessionID optionally binds a supervisor-generated fresh, canonical UUID
	// before process creation. It never requests resume. Existing owned archive
	// paths (including symlinks) are rejected without opening their contents.
	SessionID string
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
	writeMu                       sync.Mutex
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
	launch                        idleLaunch
	verifiedArchive               *archiveVerification
	firstQuestion                 *firstQuestionState
	firstAttempted                bool
}

func (s *Session) SessionID() string   { return s.id }
func (s *Session) HostVersion() string { return s.version }
func (s *Session) Models() []ModelInfo { return append([]ModelInfo(nil), s.models...) }

const supportedVersion = "2.1.287"

// Initialize a fresh owned process without sending a question. Cancellation
// owns its full lifetime, including reference preparation and completion.
func startVersion(ctx context.Context, opts Options, expectedVersion string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := opts.SessionID
	if id != "" && !canonicalSessionID(id) {
		return nil, ErrState
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
	args, env, cwd, root, err := launchOptionsForVersion(opts, expectedVersion)
	if err != nil {
		return nil, err
	}
	launch, err := freezeIdleLaunch(opts, env, cwd, expectedVersion)
	if err != nil {
		return nil, err
	}
	if id == "" {
		id, err = newUUID()
		if err != nil {
			return nil, ErrState
		}
	}
	archivePath := filepath.Join(root, providerfs.EncodeCwd(cwd), id+".jsonl")
	if err := unusedSessionArchive(archivePath); err != nil {
		return nil, err
	}
	startup, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	version, err := readVersionFor(startup, opts.Executable, cwd, env, expectedVersion)
	if err != nil {
		return nil, err
	}
	args = append(args, "--session-id", id)
	s := &Session{id: id, version: version, cwd: cwd, archiveRoot: root, launch: launch, gate: make(chan struct{}, 1), failed: make(chan struct{}), closed: make(chan struct{})}
	if err := launch.validate(); err != nil {
		return nil, err
	}
	if err := unusedSessionArchive(archivePath); err != nil {
		return nil, err
	}
	p, err := startProcess(opts.Executable, args, cwd, env, s.frame, s.fail)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.process = p
	if s.err != nil {
		// A reader can report failure before startProcess publishes its handle.
		p.requestAbort()
	}
	s.mu.Unlock()
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

func canonicalSessionID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
		} else if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func unusedSessionArchive(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// An inaccessible path also cannot certify a fresh session. Lstat rejects
	// a dangling leaf symlink instead of following it or replacing it.
	return ErrState
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
	if err := s.writeFrame(ctx, raw); err != nil {
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
	if s.err != nil && s.process != nil {
		// Wake an existing graceful shutdown without waiting for closeOnce or
		// taking ownership of process signaling/reaping from that closer.
		s.process.requestAbort()
	}
}
func (s *Session) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

func (s *Session) writeFrame(ctx context.Context, raw []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.process.write(ctx, append(raw, '\n'))
}

// Close drains and validates stdout through EOF, waits for the owned process,
// and never removes its native archive. A failed or canceled operation is killed
// before EOF can continue native work. Its original error
// remains a failure even when physical retirement succeeds; it cannot authorize
// archive proof or resume. Close is concurrent-safe and returns its first result.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		if s.pending != nil && s.err == nil {
			s.err = ErrClosed
			close(s.failed)
		}
		completed := s.firstQuestion != nil && s.firstQuestion.ordinary != nil && s.firstQuestion.completed && s.err == nil
		s.mu.Unlock()
		if completed {
			// The native result can overtake a committed permission writer's
			// post-write audit. Do not cancel that successful write ourselves.
			// Closing is already set, so no new writer can commit a response.
			s.writeMu.Lock()
			s.writeMu.Unlock()
		}
		s.mu.Lock()
		if s.firstQuestion != nil && s.firstQuestion.ordinary != nil {
			s.firstQuestion.ordinary.cancel()
		}
		// Graceful EOF is only for successful work, including no-query sessions.
		// Failed initialization or appends must not inherit
		// normal native teardown grace or continue processing on EOF either.
		abort := s.err != nil
		s.mu.Unlock()
		err := s.process.shutdown(abort)
		// Pipe shutdown interrupts committed writes. Join their audit before
		// finalizing closeErr; a late permission-write failure must not arrive
		// after successful Run/readback has already been reported. UI callbacks
		// themselves never hold this lock.
		if !completed {
			s.writeMu.Lock()
			s.writeMu.Unlock()
		}
		s.mu.Lock()
		if s.err != nil {
			// Include failures arriving during graceful shutdown or writer drain.
			// The original cause prevents success; only unconfirmed retirement
			// adds a cleanup failure to it.
			err = s.process.retirementErr
		}
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

func launchOptionsForVersion(o Options, version string) ([]string, []string, string, string, error) {
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
	if !validArgs(o.ConfigArgs, version) {
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
	args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--await-initialize", "--permission-prompt-tool", "stdio", "--permission-prompts", "host", "--prompt-suggestions", "false", "--replay-user-messages")
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	return args, env, cwd, filepath.Join(filepath.Clean(root), "projects"), nil
}

func validArgs(args []string, version string) bool {
	if len(args) > 128 || version != supportedVersion {
		return false
	}
	values := map[string]bool{"--settings": true, "--setting-sources": true, "--mcp-config": true, "--tools": true, "--allowedTools": true, "--disallowedTools": true, "--add-dir": true, "--plugin-dir": true, "--agent": true, "--system-prompt": true, "--append-system-prompt": true, "--system-prompt-file": true, "--append-system-prompt-file": true, "--effort": true, "--permission-mode": true, "--name": true}
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		if standaloneConfigFlag(name, version) && !equal {
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
		if name == "--permission-mode" {
			switch value {
			case "default", "dontAsk", "plan", "acceptEdits", "bypassPermissions", "auto", "manual":
			default:
				return false
			}
		}
	}
	return true
}

// Shared by initial launch validation and resume parsing. These options carry
// no value; they are never inserted on the caller's behalf.
func standaloneConfigFlag(name, version string) bool {
	if version != supportedVersion {
		return false
	}
	switch name {
	case "--bare", "--safe-mode", "--strict-mcp-config", "--no-chrome", "--disable-slash-commands":
		return true
	case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions":
		return true
	default:
		return false
	}
}
