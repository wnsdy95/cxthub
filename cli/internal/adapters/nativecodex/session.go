// Package nativecodex implements the local app-server protocol boundary. It
// does not attest model capacity, start model turns, or enable native delivery.
package nativecodex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrState = errors.New("native Codex session state does not allow this operation")

// Thread is the identity acknowledged by a fresh thread/start, not evidence
// that a model has accepted any input or that a TUI has attached.
type Thread struct {
	ID            string `json:"id"`
	Model         string `json:"model"`
	ModelProvider string `json:"model_provider"`
	Cwd           string `json:"cwd"`
	// SettingsHash covers the native response's effective settings, not the
	// contents of instruction files, hidden tools, capacity, or token counts.
	SettingsHash string `json:"settings_hash"`
}

type ThreadOptions struct {
	Model          string
	ModelProvider  string
	Sandbox        string
	ApprovalPolicy string
	Ephemeral      bool
}

// HistoryMessage only allows quoted conversation text. Instructions and tool
// calls require separate authority/pairing contracts and cannot enter here.
type HistoryMessage struct {
	Role string
	Text string
}

type InjectionReceipt struct {
	ThreadID           string `json:"thread_id"`
	PayloadHash        string `json:"payload_hash"`
	Items              int    `json:"items"`
	UTF8Bytes          int    `json:"utf8_bytes"`
	Acknowledged       bool   `json:"acknowledged"`
	ProviderAcceptance string `json:"provider_acceptance"`
}

// Session owns its process and fresh thread. There is deliberately no resume,
// attach-to-existing, turn/start or arbitrary RPC entry point. Generation is
// available only through a separately prepared handoff on this owned thread.
type Session struct {
	socketPath  string
	settingsRaw json.RawMessage
	injection   InjectionReceipt
	handed      bool
	rpc         *rpcClient
	process     ownedProcess
	cwd         string
	host        string
	gate        chan struct{} // cancellable lifecycle serialization, never Close
	thread      Thread
	started     bool
	injected    bool
	ephemeral   bool
	uncertain   bool
	closed      chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

func (s *Session) active() error {
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	if s.uncertain {
		return ErrState
	}
	return nil
}

func (s *Session) HostIdentity() string { return s.host }

func (s *Session) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return ErrClosed
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	}
}

func (s *Session) StartThread(ctx context.Context, opts ThreadOptions) (Thread, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.enter(ctx); err != nil {
		return Thread{}, err
	}
	defer func() { <-s.gate }()
	return s.startThread(ctx, opts)
}

// Caller holds the lifecycle gate, including any window/config binding reads.
func (s *Session) startThread(ctx context.Context, opts ThreadOptions) (Thread, error) {
	if err := s.active(); err != nil {
		return Thread{}, err
	}
	if s.started {
		return Thread{}, ErrState
	}
	if opts.Sandbox != "" && opts.Sandbox != "read-only" && opts.Sandbox != "workspace-write" && opts.Sandbox != "danger-full-access" {
		return Thread{}, ErrState
	}
	if opts.ApprovalPolicy != "" && opts.ApprovalPolicy != "untrusted" && opts.ApprovalPolicy != "on-request" && opts.ApprovalPolicy != "never" {
		return Thread{}, ErrState
	}
	params := map[string]any{"cwd": s.cwd, "ephemeral": opts.Ephemeral, "allowProviderModelFallback": false}
	if opts.Model != "" {
		params["model"] = opts.Model
	}
	if opts.ModelProvider != "" {
		params["modelProvider"] = opts.ModelProvider
	}
	if opts.Sandbox != "" {
		params["sandbox"] = opts.Sandbox
	}
	if opts.ApprovalPolicy != "" {
		params["approvalPolicy"] = opts.ApprovalPolicy
	}
	raw, err := s.rpc.call(ctx, "thread/start", params)
	if err != nil {
		s.uncertain = true
		_ = s.rpc.close()
		return Thread{}, err
	}
	thread, valid := freshThread(raw, s.cwd, opts.ModelProvider)
	if valid {
		thread.SettingsHash, valid = threadSettings(raw, thread, opts)
	}
	if !valid {
		s.uncertain = true
		_ = s.rpc.close()
		return Thread{}, fmt.Errorf("%w: invalid fresh thread identity", ErrProtocol)
	}
	s.settingsRaw = append(json.RawMessage(nil), raw...)
	s.thread = thread
	s.started = true
	s.ephemeral = opts.Ephemeral
	return s.thread, nil
}

// threadSettings binds explicit CLI choices to the acknowledged runtime. Native
// managed policy may reject or constrain a request; a mismatch must not silently
// become a different preparation runtime. Model aliases are not guessed here.
func threadSettings(raw []byte, thread Thread, opts ThreadOptions) (string, bool) {
	fields, ok := identityObject(raw, "cwd", "approvalPolicy", "approvalsReviewer", "sandbox")
	if !ok || (opts.Model != "" && thread.Model != opts.Model) {
		return "", false
	}
	var cwd, approval, reviewer string
	if json.Unmarshal(fields["cwd"], &cwd) != nil || cwd != thread.Cwd ||
		json.Unmarshal(fields["approvalsReviewer"], &reviewer) != nil || (reviewer != "user" && reviewer != "auto_review" && reviewer != "guardian_subagent") {
		return "", false
	}
	// Native config can select a granular approval object. Preserve that shape
	// in the hash but never treat it as an explicit scalar CLI approval choice.
	if json.Unmarshal(fields["approvalPolicy"], &approval) != nil {
		granular, ok := identityObject(fields["approvalPolicy"], "granular")
		if !ok || len(granular) != 1 || !validGranularApproval(granular["granular"]) {
			return "", false
		}
	} else if approval != "never" && approval != "untrusted" && approval != "on-request" {
		return "", false
	}
	if opts.ApprovalPolicy != "" && approval != opts.ApprovalPolicy {
		return "", false
	}
	sandbox, ok := identityObject(fields["sandbox"], "type")
	var kind string
	if !ok || json.Unmarshal(sandbox["type"], &kind) != nil {
		return "", false
	}
	if !validSandboxFields(kind, sandbox) {
		return "", false
	}
	expected := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}
	if kind != "readOnly" && kind != "workspaceWrite" && kind != "dangerFullAccess" && kind != "externalSandbox" {
		return "", false
	}
	if opts.Sandbox != "" && expected[opts.Sandbox] != kind {
		return "", false
	}
	// Include future native settings too, without thread IDs/timestamps which
	// would make two otherwise identical observations incomparable.
	delete(fields, "thread")
	canonical, err := json.Marshal(fields)
	if err != nil {
		return "", false
	}
	// Normalize nested object ordering too; a response serializer changing its
	// map order is not a runtime configuration change. Keep numeric precision.
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if decoder.Decode(&normalized) != nil {
		return "", false
	}
	canonical, err = json.Marshal(normalized)
	if err != nil {
		return "", false
	}
	hash := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(hash[:]), true
}

func validGranularApproval(raw json.RawMessage) bool {
	fields, ok := identityObject(raw, "mcp_elicitations", "rules", "sandbox_approval")
	if !ok {
		return false
	}
	for key, value := range fields {
		switch key {
		case "mcp_elicitations", "rules", "sandbox_approval", "request_permissions", "skill_approval":
			if string(value) != "true" && string(value) != "false" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validSandboxFields(kind string, fields map[string]json.RawMessage) bool {
	for key, value := range fields {
		if key == "type" {
			continue
		}
		switch key {
		case "networkAccess":
			if kind == "externalSandbox" {
				var network string
				if json.Unmarshal(value, &network) != nil || (network != "enabled" && network != "restricted") {
					return false
				}
			} else if (kind != "readOnly" && kind != "workspaceWrite") || (string(value) != "true" && string(value) != "false") {
				return false
			}
		case "excludeSlashTmp", "excludeTmpdirEnvVar":
			if kind != "workspaceWrite" || (string(value) != "true" && string(value) != "false") {
				return false
			}
		case "writableRoots":
			var roots []string
			if kind != "workspaceWrite" || json.Unmarshal(value, &roots) != nil || roots == nil {
				return false
			}
			for _, root := range roots {
				if root == "" || strings.ContainsRune(root, 0) || !filepath.IsAbs(root) {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// identityObject rejects duplicate fields and case aliases before any struct
// decoding can silently choose one of two conflicting identity claims. Extra
// native fields remain forward compatible, but required fields are exact.
func identityObject(raw []byte, fields ...string) (map[string]json.RawMessage, bool) {
	object, ok := rpcObject(raw)
	if !ok {
		return nil, false
	}
	for _, field := range fields {
		if _, present := object[field]; !present {
			return nil, false
		}
		for key := range object {
			if key != field && strings.EqualFold(key, field) {
				return nil, false
			}
		}
	}
	return object, true
}

func freshThread(raw []byte, cwd, provider string) (Thread, bool) {
	result, ok := identityObject(raw, "model", "modelProvider", "thread")
	if !ok {
		return Thread{}, false
	}
	identity, ok := identityObject(result["thread"], "id", "cwd", "turns")
	if !ok {
		return Thread{}, false
	}
	var thread Thread
	var turns []json.RawMessage
	if json.Unmarshal(identity["id"], &thread.ID) != nil || !validID(thread.ID) ||
		json.Unmarshal(identity["cwd"], &thread.Cwd) != nil || thread.Cwd != cwd ||
		json.Unmarshal(identity["turns"], &turns) != nil || turns == nil || len(turns) != 0 ||
		json.Unmarshal(result["model"], &thread.Model) != nil || thread.Model == "" ||
		json.Unmarshal(result["modelProvider"], &thread.ModelProvider) != nil || thread.ModelProvider == "" ||
		(provider != "" && thread.ModelProvider != provider) {
		return Thread{}, false
	}
	return thread, true
}

// InjectHistory is one-shot. A failed or lost ACK is ambiguous; it must never
// be retried on the same thread, since the first request may have persisted.
func (s *Session) InjectHistory(ctx context.Context, messages []HistoryMessage) (InjectionReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.enter(ctx); err != nil {
		return InjectionReceipt{}, err
	}
	defer func() { <-s.gate }()
	if err := s.active(); err != nil {
		return InjectionReceipt{}, err
	}
	if !s.started || s.injected || len(messages) == 0 || len(messages) > 4096 {
		return InjectionReceipt{}, ErrState
	}
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type item struct {
		Type    string    `json:"type"`
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	items := make([]item, 0, len(messages))
	bytes := 0
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			return InjectionReceipt{}, ErrState
		}
		if message.Text == "" || !utf8.ValidString(message.Text) || len(message.Text) > maxMessageBytes-bytes {
			return InjectionReceipt{}, ErrState
		}
		bytes += len(message.Text)
		kind := "input_text"
		if message.Role == "assistant" {
			kind = "output_text"
		}
		items = append(items, item{"message", message.Role, []content{{kind, message.Text}}})
	}
	encoded, err := json.Marshal(items)
	if err != nil || len(encoded) > maxMessageBytes-1024 {
		return InjectionReceipt{}, ErrState
	}
	hash := sha256.Sum256(encoded)
	receipt := InjectionReceipt{ThreadID: s.thread.ID, PayloadHash: "sha256:" + hex.EncodeToString(hash[:]), Items: len(items), UTF8Bytes: bytes, ProviderAcceptance: "unverified"}
	raw, err := s.rpc.call(ctx, "thread/inject_items", map[string]any{"threadId": s.thread.ID, "items": json.RawMessage(encoded)})
	if err != nil {
		s.uncertain = true
		_ = s.rpc.close()
		return receipt, err
	}
	var ack map[string]json.RawMessage
	if json.Unmarshal(raw, &ack) != nil || ack == nil || len(ack) != 0 {
		s.uncertain = true
		_ = s.rpc.close()
		return receipt, fmt.Errorf("%w: invalid injection acknowledgement", ErrProtocol)
	}
	s.injected, receipt.Acknowledged = true, true
	s.injection = receipt
	return receipt, nil
}

// Close interrupts reads before waiting for the owned process. It never needs
// the operation mutex, so cancellation cannot deadlock behind an awaiting RPC.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.rpc.close()
		s.closeErr = s.process.stop()
	})
	return s.closeErr
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}
