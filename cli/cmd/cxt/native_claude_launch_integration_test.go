//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
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

func TestNativeClaudeLaunchNamedSessionThroughAdapter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		reject bool
	}{
		{"short", []string{"-n", "review \ud55c\uae00 = one"}, false},
		{"long", []string{"--name", "review \ud55c\uae00 = one"}, false},
		{"equals", []string{"--name=review \ud55c\uae00 = one"}, false},
		{"debug-short-rejected", []string{"-d"}, true},
		{"debug-long-rejected", []string{"--debug"}, true},
		{"debug-filter-rejected", []string{"--debug=api,hooks"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			recorder := filepath.Join(root, "launch.jsonl")
			args := append(append([]string{}, tc.args...), "--", "PRIVATE_QUESTION")
			bound, err := bindNativeClaudeLaunch(delivcli.ProviderLaunchRequest{Cwd: root, Executable: exe, Intent: delivcli.LaunchIntent{
				Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000, ProviderArgs: args,
			}})
			if err != nil {
				t.Fatal("shared binder rejected invocation:", err)
			}
			bound.options.Env = []string{"HOME=" + root, "TMPDIR=" + root, "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "config"), "CXT_CLAUDE_LAUNCH_TEST_HELPER=1", "CXT_CLAUDE_LAUNCH_TEST_RECORD=" + recorder, "GORACE=atexit_sleep_ms=0"}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			exchange, err := nativeclaude.StartFirstExchange(ctx, bound.options)
			if tc.reject {
				if exchange != nil {
					_ = exchange.Close()
				}
				if !errors.Is(err, nativeclaude.ErrState) {
					t.Fatal("debug must remain unsupported by the real adapter:", err)
				}
				if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unsupported debug option launched a subprocess")
				}
				return
			}
			if err != nil {
				t.Fatal("bound session name rejected by the real adapter:", err)
			}
			defer exchange.Close()
			const nameArg = "--name=review \ud55c\uae00 = one"
			resume, err := exchange.ResumeArguments()
			if err != nil || !reflect.DeepEqual(resume, []string{nameArg, "--resume", exchange.OwnedArchivePath()}) {
				t.Fatal("session name changed in resume arguments:", err)
			}
			if err := exchange.Close(); err != nil {
				t.Fatal("synthetic initialization did not close cleanly:", err)
			}
			f, err := os.Open(recorder)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var records []nativeClaudeLaunchTestRecord
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				var record nativeClaudeLaunchTestRecord
				if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
			}
			if scanner.Err() != nil || len(records) != 3 || !slices.Equal(records[0].Args, []string{"--version"}) || !slices.Contains(records[1].Args, nameArg) || slices.Contains(records[1].Args, "PRIVATE_QUESTION") || len(records[2].Frame) == 0 {
				t.Fatal("expected exactly one version probe, named launch, and initialization without a question")
			}
			if bound.prompt.Text() != "PRIVATE_QUESTION" || exchange.HostVersion() != "2.1.287" {
				t.Fatal("binding changed the question or accepted the wrong native version")
			}
		})
	}
}
