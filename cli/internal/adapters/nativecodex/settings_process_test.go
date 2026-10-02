//go:build darwin || linux

package nativecodex

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRuntimeMismatchStopsBeforeInjection(t *testing.T) {
	for _, mode := range []string{"wrong-model", "wrong-sandbox", "wrong-approval"} {
		t.Run(mode, func(t *testing.T) {
			s, trace := startFixture(t, mode)
			_, err := s.StartThread(context.Background(), ThreadOptions{Model: "fixture-resolved", ModelProvider: "fixture-provider", Sandbox: "read-only", ApprovalPolicy: "untrusted"})
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("runtime mismatch error: %v", err)
			}
			if _, err = s.InjectHistory(context.Background(), []HistoryMessage{{Role: "user", Text: "must not inject"}}); err == nil {
				t.Fatal("mismatched runtime accepted history")
			}
			raw, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "thread/inject_items") {
				t.Fatal("history sent after runtime mismatch")
			}
		})
	}
}
