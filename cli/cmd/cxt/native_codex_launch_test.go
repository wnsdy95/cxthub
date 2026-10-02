package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func nativeLaunchRequest(args ...string) delivcli.ProviderLaunchRequest {
	return delivcli.ProviderLaunchRequest{Executable: "/fixture/codex", Cwd: filepath.Join(os.TempDir(), "cxt-native-fixture", "work"), Intent: delivcli.LaunchIntent{Provider: domain.ProviderCodex, ProviderArgs: args}}
}

func TestNativeLaunchBindsOneParsedIntent(t *testing.T) {
	request := nativeLaunchRequest("-c", `model="config-model"`, "-mexplicit", "--config=model=second-config", "--disable", "shell_tool", "--enable=shell_tool", "-sread-only", "--ask-for-approval=untrusted", "-C"+filepath.Join(os.TempDir(), "cxt-native-fixture", "work"), "--strict-config", "--no-alt-screen", "--no-daemon", "--", "--yolo literal prompt")
	bound, err := bindNativeCodexLaunch(request)
	if err != nil {
		t.Fatal(err)
	}
	if bound.thread.Model != "explicit" || bound.thread.Sandbox != "read-only" || bound.thread.ApprovalPolicy != "untrusted" || !bound.process.StrictConfig || bound.process.Cwd != request.Cwd || bound.prompt != "--yolo literal prompt" {
		t.Fatal("launch intent changed")
	}
	want := []string{"-c", `model="config-model"`, "--config", "model=second-config", "--disable", "shell_tool", "--enable", "shell_tool"}
	if !reflect.DeepEqual(bound.process.ConfigArgs, want) {
		t.Fatal("native config precedence changed")
	}
	before := bound.intent
	request.Intent.ProviderArgs[1] = "PRIVATE_CHANGED"
	if !reflect.DeepEqual(bound.process.ConfigArgs, want) || bound.intent != before {
		t.Fatal("caller mutated bound launch")
	}
	changed, err := bindNativeCodexLaunch(request)
	if err != nil || changed.intent == before {
		t.Fatal("different input not distinguished")
	}
}

func TestNativeLaunchPreservesBypassPrecedenceAndDefaultModel(t *testing.T) {
	for _, args := range [][]string{
		{"--yolo", "-s", "read-only"},
		{"-s", "read-only", "--dangerously-bypass-approvals-and-sandbox"},
	} {
		bound, err := bindNativeCodexLaunch(nativeLaunchRequest(args...))
		if err != nil || bound.thread.Sandbox != "danger-full-access" || bound.thread.ApprovalPolicy != "never" {
			t.Fatal("explicit bypass precedence changed")
		}
		if bound.thread.Model != "" {
			t.Fatal("default/profile model was invented")
		}
	}
	bound, err := bindNativeCodexLaunch(nativeLaunchRequest("-c", `web_search="disabled"`, "--search"))
	if err != nil || bound.process.ConfigArgs[len(bound.process.ConfigArgs)-1] != `web_search="live"` {
		t.Fatal("explicit native search precedence changed")
	}
}

func TestNativeLaunchRejectsUnmappedIntentWithoutLeaking(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "PRIVATE_PROFILE"}, {"--add-dir", "/PRIVATE_PATH"},
		{"--oss"}, {"--local-provider", "PRIVATE_PROVIDER"},
		{"-i", "PRIVATE_IMAGE"}, {"--approve-for-me"}, {"--dangerously-bypass-hook-trust"},
		{"--remote", "PRIVATE_URL"}, {"exec", "PRIVATE_TASK"},
		{"-a", "PRIVATE_POLICY"}, {"-s", "PRIVATE_SANDBOX"},
		{"first", "PRIVATE_SECOND"}, {"--PRIVATE_UNKNOWN"}, {"-m", "PRIVATE\nMODEL"},
		{"--model="}, {"-m", "first", "--model=second"}, {"--sandbox="},
		{"--yolo", "-s", "PRIVATE_INVALID"}, {"--yolo", "-a", "untrusted"}, {"-a", "never", "--yolo"},
	} {
		_, err := bindNativeCodexLaunch(nativeLaunchRequest(args...))
		if err == nil {
			t.Fatalf("unsupported option accepted: %q", args[0])
		}
		if strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("launch error disclosed private input")
		}
	}
	bound, err := bindNativeCodexLaunch(nativeLaunchRequest("-c", `model_providers.local.http_headers={Private="PRIVATE_CONFIG"}`, "PRIVATE_PROMPT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(bound)
	for _, s := range []string{string(raw), fmt.Sprint(bound), fmt.Sprintf("%+v", bound), fmt.Sprintf("%#v", bound)} {
		if strings.Contains(s, "PRIVATE") {
			t.Fatal("bound input leaked through routine formatting")
		}
	}
}
