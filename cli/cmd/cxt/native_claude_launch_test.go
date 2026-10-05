package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestNativeClaudeLaunchUsesSharedParser(t *testing.T) {
	req := delivcli.ProviderLaunchRequest{Cwd: t.TempDir(), Executable: "claude", Intent: delivcli.LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 800000, ProviderArgs: []string{"--model=sonnet", "--permission-mode", "plan", "--settings={\"private\":true}", "--", "question-secret"}}}
	b, err := bindNativeClaudeLaunch(req)
	if err != nil {
		t.Fatal(err)
	}
	if b.options.Model != "sonnet" || b.prompt.Text() != "question-secret" || !reflect.DeepEqual(b.options.ConfigArgs, []string{"--permission-mode=plan", "--settings={\"private\":true}"}) {
		t.Fatal("launch settings changed")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", b, b), "secret") {
		t.Fatal("private question rendered")
	}
	for _, args := range [][]string{{"--model=a", "--model=b"}, {"--add-dir", "a", "b"}, {"--print", "question"}, {"--resume", "previous"}, {"one", "two"}} {
		req.Intent.ProviderArgs = args
		if _, err := bindNativeClaudeLaunch(req); err == nil {
			t.Fatalf("accepted ambiguous or non-fresh invocation: %v", args)
		}
	}
}

func TestNativeClaudeLaunchShortValueDoesNotAcquireEquals(t *testing.T) {
	req := delivcli.ProviderLaunchRequest{Cwd: t.TempDir(), Executable: "claude", Intent: delivcli.LaunchIntent{Provider: domain.ProviderClaude, Pull: true, ContextBudget: 200000, ProviderArgs: []string{"-n", "review", "--", "question"}}}
	got, err := bindNativeClaudeLaunch(req)
	if err != nil || !reflect.DeepEqual(got.options.ConfigArgs, []string{"--name=review"}) {
		t.Fatalf("args=%v error=%v", got.options.ConfigArgs, err)
	}
}
