package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
)

func scriptedClaudeConsole(lines ...string) (*nativeClaudeConsole, *bytes.Buffer) {
	var out bytes.Buffer
	read := func(context.Context) (string, error) {
		if len(lines) == 0 {
			return "", io.EOF
		}
		line := lines[0]
		lines = lines[1:]
		return line, nil
	}
	return newNativeClaudeConsole(read, &out), &out
}

func TestNativeClaudeConsolePermissionIsExplicitOnce(t *testing.T) {
	for _, tc := range []struct {
		line string
		want nativeclaude.ToolPermissionDecision
	}{{"yes", nativeclaude.AllowOnce}, {"no", nativeclaude.DenyOnce}, {"", nativeclaude.DenyOnce}} {
		c, out := scriptedClaudeConsole(tc.line)
		decision, err := c.permission(context.Background(), nativeclaude.ToolPermissionRequest{ToolName: "Bash\x1b[2J", Input: []byte(`{"command":"printf '\u001b'"}`), Description: "clear\x1b[2J", DecisionReason: "shown reason"})
		if err != nil || decision != tc.want {
			t.Fatalf("decision=%v err=%v", decision, err)
		}
		if bytes.ContainsRune(out.Bytes(), '\x1b') || !strings.Contains(out.String(), "shown reason") || !strings.Contains(out.String(), "command") {
			t.Fatal("unsafe or incomplete permission display")
		}
	}
	c, _ := scriptedClaudeConsole("allow", "yes")
	if decision, err := c.permission(context.Background(), nativeclaude.ToolPermissionRequest{Input: []byte(`{}`)}); err != nil || decision != nativeclaude.AllowOnce {
		t.Fatal("retry did not require explicit answer", err)
	}
	if decision, err := c.permission(context.Background(), nativeclaude.ToolPermissionRequest{Input: []byte(`{}`)}); !errors.Is(err, io.EOF) || decision != 0 {
		t.Fatal("previous approval reused")
	}
}

func TestNativeClaudeConsoleCancellationDoesNotApproveOrBlockQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	c := newNativeClaudeConsole(func(ctx context.Context) (string, error) { close(entered); <-ctx.Done(); return "yes", ctx.Err() }, io.Discard)
	done := make(chan error, 1)
	go func() {
		decision, err := c.permission(ctx, nativeclaude.ToolPermissionRequest{Input: []byte(`{}`)})
		if decision != 0 {
			err = errors.New("late decision")
		}
		done <- err
	}()
	<-entered
	queued, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := c.permission(queued, nativeclaude.ToolPermissionRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queued cancellation", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("withdrawn dialog leaked")
	}
	if len(c.gate) != 0 {
		t.Fatal("terminal remained locked")
	}
}

func TestNativeClaudeConsoleQuestionsPreserveSelectionAndCustomText(t *testing.T) {
	c, _ := scriptedClaudeConsole("1,1", "2,1", "text:custom, untouched", "/skip")
	req := nativeclaude.UserQuestionRequest{Questions: []nativeclaude.UserQuestion{
		{Question: "multi", MultiSelect: true, Options: []nativeclaude.UserQuestionOption{{Label: "first"}, {Label: "second"}}},
		{Question: "single", Options: []nativeclaude.UserQuestionOption{{Label: "one"}, {Label: "two"}}},
		{Question: "skip", Options: []nativeclaude.UserQuestionOption{{Label: "a"}, {Label: "b"}}},
	}}
	got, err := c.questions(context.Background(), req)
	want := map[string][]string{"multi": {"second", "first"}, "single": {"custom, untouched"}}
	if err != nil || !reflect.DeepEqual(got.Answers, want) {
		t.Fatalf("answers=%v error=%v", got, err)
	}
	if _, ok := nativeConsoleSelection("1,2", req.Questions[1]); ok {
		t.Fatal("single question accepted multiple answers")
	}
}

func TestNativeClaudeConsoleQuestionDoesNotConsumeNextDialog(t *testing.T) {
	c, _ := scriptedClaudeConsole("first line", "second line", "/submit", "no")
	p, err := c.question(context.Background())
	if err != nil || p.Text() != "first line\nsecond line" {
		t.Fatal("question changed", err)
	}
	if answer, err := c.permission(context.Background(), nativeclaude.ToolPermissionRequest{Input: []byte(`{}`)}); err != nil || answer != nativeclaude.DenyOnce {
		t.Fatal("next dialog input was consumed", err)
	}
	c, _ = scriptedClaudeConsole("/cancel")
	if _, err := c.question(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel", err)
	}
}
