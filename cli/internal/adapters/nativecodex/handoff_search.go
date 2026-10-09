package nativecodex

import (
	"bytes"
	"context"
	"encoding/json"
)

// Keep the original subscribed RPC alive and confirm the owned thread is still
// loaded before forwarding a resume. Never turn an inherited setting into a
// cold resume after losing the runtime that supplied it.
func (s *Session) checkHandoffResumeOwner(ctx context.Context, data []byte) error {
	frame, ok := rpcObject(data)
	if !ok || !stringEquals(frame["method"], "thread/resume") {
		return nil // complete frame validation already ran in observe
	}
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	raw, err := s.rpc.call(ctx, "thread/loaded/list", map[string]any{})
	if err != nil {
		return err
	}
	result, ok := identityObject(raw, "data")
	var ids []string
	if !ok || json.Unmarshal(result["data"], &ids) != nil {
		return handoffError("loaded thread identity unavailable")
	}
	for _, id := range ids {
		if id == s.thread.ID {
			return nil
		}
	}
	return handoffError("prepared thread is no longer loaded")
}

// An empty setting means native owns resolution (including legacy features,
// requirements, provider capabilities and permissions). It is not "disabled".
func handoffSearchSetting(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true
	}
	var mode string
	if json.Unmarshal(raw, &mode) != nil || !validHandoffSearch(mode) {
		return "", false
	}
	return mode, true
}

func validHandoffSearch(mode string) bool {
	return mode == "cached" || mode == "live" || mode == "disabled" || mode == "indexed"
}

func (p *handoffProtocol) preservesSearch(raw json.RawMessage) bool {
	mode, ok := handoffSearchSetting(raw)
	return ok && mode != "" && (p.search == "" || mode == p.search)
}

// Called only after observe has validated the complete client frame. Stock
// 0.157.1's TUI includes its resolved search value in thread/resume. When the
// original setting is unset, do not apply that second client's default to the
// already prepared thread. Omit this one override; native retains its own
// configuration. Never rely on loaded-thread overrides being ignored: native
// may cold-reload an idle unsubscribed thread when overrides differ.
func (p *handoffProtocol) forwardResume(data []byte) ([]byte, error) {
	if p.search != "" {
		return data, nil
	}
	frame, ok := rpcObject(data)
	if !ok {
		return nil, handoffError("invalid forwarding frame")
	}
	if !stringEquals(frame["method"], "thread/resume") {
		return data, nil
	}
	params, ok := rpcObject(frame["params"])
	if !ok {
		return nil, handoffError("invalid resume parameters")
	}
	if _, exists := params["config"]; !exists {
		return data, nil
	}
	delete(params, "config") // validation permits only the search field here
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, handoffError("invalid resume parameters")
	}
	frame["params"] = raw
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, handoffError("invalid resume frame")
	}
	return encoded, nil
}
