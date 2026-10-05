package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
)

func TestNativeClaudeRetirementSeparatesCancellationFromCleanupFailure(t *testing.T) {
	if err := nativeClaudeRetirementError(errors.Join(context.Canceled)); err != nil {
		t.Fatal("clean canceled retirement rejected", err)
	}
	for _, cause := range []error{nativeclaude.ErrCleanup, nativeclaude.ErrProtocol, nativeclaude.ErrInteraction, context.DeadlineExceeded} {
		err := nativeClaudeRetirementError(errors.Join(context.Canceled, cause))
		if !errors.Is(err, cause) {
			t.Fatal("retirement failure erased", cause)
		}
	}
}

func TestNativeClaudeConsoleAnswerCannotClearTerminal(t *testing.T) {
	got := nativeConsoleText("line\n\tanswer\x1b[2J\a")
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\a') || !strings.HasPrefix(got, "line\n\tanswer") {
		t.Fatal("native control sequence rendered", got)
	}
}
