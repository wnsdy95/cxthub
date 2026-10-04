package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
)

const nativeHandoffTestID = "019abcde-1234-7123-8123-123456789abc"

func TestNativeHandoffPreservesBoundRootOptionsWithoutPrompt(t *testing.T) {
	const question = "PRIVATE_QUESTION --remote unix:// --yolo"
	req := nativeLaunchRequest("-c", `model="config-model"`, "-mexplicit", "--config=model=second-config",
		"--disable", "shell_tool", "--enable=shell_tool", "-sread-only", "--ask-for-approval=untrusted",
		"--strict-config", "--no-alt-screen", "--no-daemon", "-Crelative", "--", question)
	bound, err := bindNativeCodexLaunch(req)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately nonexistent paths: mapping must not perform filesystem I/O.
	thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: filepath.Join(req.Cwd, "canonical")}
	endpoint := "unix://" + filepath.Join(req.Cwd, "private.sock")
	want := []string{"--remote", endpoint, `--config=model="config-model"`, "--model=explicit",
		"--config=model=second-config", "--disable=shell_tool", "--enable=shell_tool",
		"--strict-config", "--no-alt-screen", "--cd=" + thread.Cwd,
		"resume", thread.ID}
	got, err := bound.tuiResumeArgs(endpoint, thread)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("handoff mapping: args=%q err=%v", got, err)
	}
	if !bound.prompt.Present() || bound.prompt.Text() != question {
		t.Fatal("mapping changed the withheld prompt")
	}
	// Both the original request and each returned argv must be independent.
	intent := bound.intent
	req.Intent.ProviderArgs[1] = "PRIVATE_MUTATED_CONFIG"
	req.Intent.ProviderArgs[len(req.Intent.ProviderArgs)-1] = "PRIVATE_MUTATED_QUESTION"
	got[2] = "PRIVATE_MUTATED_RESULT"
	again, err := bound.tuiResumeArgs(endpoint, thread)
	if err != nil || !reflect.DeepEqual(again, want) || bound.intent != intent {
		t.Fatal("handoff arguments alias mutable input or output")
	}
	raw, err := json.Marshal(bound)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{strings.Join(again, "\n"), string(raw), fmt.Sprint(bound), fmt.Sprintf("%+v", bound), fmt.Sprintf("%#v", bound)} {
		if strings.Contains(output, "PRIVATE_") {
			t.Fatal("private prompt or mutated input escaped bound launch")
		}
	}
}

func TestNativeHandoffCwdAndTerminalChoices(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		alt  bool
	}{
		{"defaults", nil, false},
		{"private server replaces no daemon", []string{"--no-daemon"}, false},
		{"inline terminal", []string{"--no-alt-screen"}, true},
		{"both", []string{"--no-daemon", "--no-alt-screen"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cd := range [][]string{nil, {"-C", "relative"}, {"--cd=relative"}} {
				req := nativeLaunchRequest(append(append([]string(nil), tc.args...), cd...)...)
				bound, err := bindNativeCodexLaunch(req)
				if err != nil {
					t.Fatal(err)
				}
				thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: filepath.Join(req.Cwd, "native-canonical")}
				endpoint := "unix://" + filepath.Join(req.Cwd, "tui.sock")
				want := []string{"--remote", endpoint}
				if tc.alt {
					want = append(want, "--no-alt-screen")
				}
				want = append(want, "--cd="+thread.Cwd, "resume", thread.ID)
				got, err := bound.tuiResumeArgs(endpoint, thread)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("cwd/terminal mapping: args=%q err=%v", got, err)
				}
			}
		})
	}
}

func TestNativeHandoffConsumesAcknowledgedPermissionFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		sandbox  string
		approval string
	}{
		{"default", nil, "", ""},
		{"short sandbox", []string{"-sread-only"}, "read-only", ""},
		{"long sandbox", []string{"--sandbox=workspace-write"}, "workspace-write", ""},
		{"short approval", []string{"-a", "on-request"}, "", "on-request"},
		{"long approval", []string{"--ask-for-approval=never"}, "", "never"},
		{"combined", []string{"-s", "read-only", "-auntrusted"}, "read-only", "untrusted"},
		{"bypass alias", []string{"--yolo"}, "danger-full-access", "never"},
		{"bypass long", []string{"--dangerously-bypass-approvals-and-sandbox"}, "danger-full-access", "never"},
		{"bypass first", []string{"--yolo", "-sread-only"}, "danger-full-access", "never"},
		{"bypass last", []string{"--sandbox=read-only", "--yolo"}, "danger-full-access", "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := nativeLaunchRequest(tc.args...)
			bound, err := bindNativeCodexLaunch(req)
			if err != nil {
				t.Fatal(err)
			}
			thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: req.Cwd}
			endpoint := "unix://" + filepath.Join(req.Cwd, "tui.sock")
			want := []string{"--remote", endpoint, "--cd=" + thread.Cwd, "resume", thread.ID}
			got, err := bound.tuiResumeArgs(endpoint, thread)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("permission flags escaped into remote resume: args=%q err=%v", got, err)
			}
			if bound.thread.Sandbox != tc.sandbox || bound.thread.ApprovalPolicy != tc.approval {
				t.Fatal("consuming TUI flags changed the owned thread/start permissions")
			}
		})
	}
}

func TestNativeHandoffSearchPreservesExplicitNativeOverrides(t *testing.T) {
	for _, mode := range []string{"", "disabled", "cached", "indexed", "live"} {
		for _, search := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/search=%t", mode, search), func(t *testing.T) {
				var original, root, config []string
				if search {
					// Even when first, --search overrides the later -c value.
					original = append(original, "--search")
				}
				if mode != "" {
					value := `web_search="` + mode + `"`
					original = append(original, "-c", value)
					root = append(root, "--config="+value)
					config = append(config, "-c", value)
				}
				if search {
					root = append(root, `--config=web_search="live"`)
					config = append(config, "-c", `web_search="live"`)
				}
				req := nativeLaunchRequest(original...)
				bound, err := bindNativeCodexLaunch(req)
				if err != nil {
					t.Fatal(err)
				}
				thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: req.Cwd}
				endpoint := "unix://" + filepath.Join(req.Cwd, "tui.sock")
				want := append([]string{"--remote", endpoint}, root...)
				want = append(want, "--cd="+thread.Cwd, "resume", thread.ID)
				got, err := bound.tuiResumeArgs(endpoint, thread)
				if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(bound.process.ConfigArgs, config) {
					t.Fatalf("preparation/handoff search mapping diverged: args=%q err=%v", got, err)
				}
			})
		}
	}
}

func TestNativeHandoffPromptAndOptionRoles(t *testing.T) {
	for _, prompt := range [][]string{nil, {""}, {"--", "--yolo"}, {"--", "resume"}, {"PRIVATE\r\nQUESTION\r"}} {
		original := append([]string{"--model=-literal", "--yolo", "-sread-only"}, prompt...)
		req := nativeLaunchRequest(original...)
		bound, err := bindNativeCodexLaunch(req)
		if err != nil {
			t.Fatal(err)
		}
		thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: req.Cwd}
		endpoint := "unix://" + filepath.Join(req.Cwd, "tui.sock")
		want := []string{"--remote", endpoint, "--model=-literal", "--cd=" + thread.Cwd, "resume", thread.ID}
		got, err := bound.tuiResumeArgs(endpoint, thread)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("positional input escaped into handoff: args=%q err=%v", got, err)
		}
	}
}

func TestNativeHandoffRejectsInvalidEndpointAndIdentity(t *testing.T) {
	req := nativeLaunchRequest("PRIVATE_QUESTION")
	bound, err := bindNativeCodexLaunch(req)
	if err != nil {
		t.Fatal(err)
	}
	thread := nativecodex.Thread{ID: nativeHandoffTestID, Cwd: req.Cwd}
	endpoint := "unix://" + filepath.Join(req.Cwd, "tui.sock")
	assertRejected := func(n nativeCodexLaunch, url string, target nativecodex.Thread) {
		t.Helper()
		args, err := n.tuiResumeArgs(url, target)
		if err == nil || args != nil {
			t.Fatal("invalid handoff returned launch arguments")
		}
		if strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("handoff error disclosed private input")
		}
	}
	for _, bad := range []string{
		"", "unix://", "unix:///", "unix://PRIVATE", "unix://PRIVATE/path", "unix:/PRIVATE.sock",
		"ws://PRIVATE:1234", "https://PRIVATE", "unix:////PRIVATE.sock", "unix:///PRIVATE/../tui.sock",
		"unix:///PRIVATE.sock/", "unix:///PRIVATE.sock?token=secret", "unix:///PRIVATE.sock?",
		"unix:///PRIVATE.sock#fragment", "unix:///PRIVATE.sock#", "unix://user@PRIVATE/tui.sock",
		"unix:///%50RIVATE.sock", "unix:///PRIVATE\\tui.sock", "unix:///PRIVATE\x00.sock",
		"unix:///PRIVATE\r.sock", "unix:///PRIVATE\n.sock", "unix:///PRIVATE\t.sock",
	} {
		assertRejected(bound, bad, thread)
	}
	for _, id := range []string{"", "PRIVATE", "--PRIVATE", "../PRIVATE", nativeHandoffTestID + "\n", strings.Repeat("a", 129)} {
		target := thread
		target.ID = id
		assertRejected(bound, endpoint, target)
	}
	for _, cwd := range []string{"", "PRIVATE/relative", req.Cwd + "/../PRIVATE", req.Cwd + "/", req.Cwd + "/PRIVATE\x00"} {
		target := thread
		target.Cwd = cwd
		assertRejected(bound, endpoint, target)
	}
	assertRejected(nativeCodexLaunch{}, endpoint, thread)
}
