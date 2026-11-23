//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"
)

// Re-exec only this test binary, with a private environment and finite lifetime.
// No installed native executable or provider is used by these tests.
func init() {
	if os.Getenv("CXT_CLAUDE_LAUNCH_TEST_HELPER") == "1" {
		time.AfterFunc(8*time.Second, func() { os.Exit(98) })
		os.Exit(nativeClaudeLaunchTestHelper())
	}
}

type nativeClaudeLaunchTestRecord struct {
	Args  []string        `json:"args,omitempty"`
	Frame json.RawMessage `json:"frame,omitempty"`
}

func nativeClaudeLaunchTestHelper() int {
	f, err := os.OpenFile(os.Getenv("CXT_CLAUDE_LAUNCH_TEST_RECORD"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 90
	}
	defer f.Close()
	record := json.NewEncoder(f)
	if err := record.Encode(nativeClaudeLaunchTestRecord{Args: os.Args[1:]}); err != nil {
		return 91
	}
	if slices.Equal(os.Args[1:], []string{"--version"}) {
		fmt.Println("2.1.287 (Claude Code)")
		return 0
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if err := record.Encode(nativeClaudeLaunchTestRecord{Frame: scanner.Bytes()}); err != nil {
			return 92
		}
		var frame struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Type != "control_request" || frame.Request.Subtype != "initialize" || frame.RequestID == "" {
			return 93 // Never accept a question, tool call, or permission reply.
		}
		response := map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": frame.RequestID,
			"response": map[string]any{"models": []any{map[string]any{"value": "sonnet", "resolvedModel": "fixture-model"}}},
		}}
		if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
			return 94
		}
	}
	if scanner.Err() != nil {
		return 95
	}
	return 0
}
