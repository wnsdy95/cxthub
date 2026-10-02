package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// nativeCodexLaunch owns parsed intent, not a capacity attestation. It is not
// wired to public launch until exact capacity and interactive handoff exist.
// Config and the initial task may contain secrets: keep them out of receipts,
// JSON, and routine formatting. The prompt must enter later host accounting;
// creating this fresh runtime never submits it or starts a model turn.
type nativeCodexLaunch struct {
	process nativecodex.Options
	thread  nativecodex.ThreadOptions
	prompt  string
	intent  domain.ContentHash
}

func (nativeCodexLaunch) String() string     { return "native Codex launch (private input)" }
func (n nativeCodexLaunch) GoString() string { return n.String() }

// bindNativeCodexLaunch consumes the same parser used by the supervisor. Do not
// introduce another argv parser or emulate profile TOML loading here.
func bindNativeCodexLaunch(req delivcli.ProviderLaunchRequest) (nativeCodexLaunch, error) {
	var bound nativeCodexLaunch
	if req.Intent.Provider != domain.ProviderCodex || req.Executable == "" || !filepath.IsAbs(req.Cwd) {
		return bound, fmt.Errorf("native Codex launch requires an executable and an absolute selected directory")
	}
	details, err := req.ArgumentDetails()
	if err != nil {
		// The classifier can include unknown option names supplied by the user.
		return bound, fmt.Errorf("native Codex launch arguments could not be classified")
	}
	if details.Mode != "fresh interactive session" || len(details.Prompts) > 1 {
		return bound, fmt.Errorf("native Codex preparation requires a fresh session with at most one initial prompt")
	}
	bound.process = nativecodex.Options{Executable: req.Executable, Cwd: req.Cwd}
	bound.thread.Model = details.Model
	if len(details.Prompts) == 1 {
		bound.prompt = details.Prompts[0]
	}
	search, bypass := false, false
	seen := map[string]bool{}
	for _, option := range details.Options {
		value := ""
		if len(option.Values) == 1 {
			value = option.Values[0]
		}
		canonical := map[string]string{"-m": "--model", "-C": "--cd", "-s": "--sandbox", "-a": "--ask-for-approval", "-p": "--profile", "--yolo": "--dangerously-bypass-approvals-and-sandbox"}[option.Name]
		if canonical == "" {
			canonical = option.Name
		}
		repeatable := canonical == "-c" || canonical == "--config" || canonical == "--enable" || canonical == "--disable"
		if seen[canonical] && !repeatable {
			return nativeCodexLaunch{}, fmt.Errorf("native Codex launch repeats a non-repeatable option")
		}
		seen[canonical] = true
		if repeatable || canonical == "--model" || canonical == "--sandbox" || canonical == "--ask-for-approval" || canonical == "--cd" || canonical == "--profile" {
			if len(option.Values) != 1 || value == "" {
				return nativeCodexLaunch{}, fmt.Errorf("native Codex launch is missing an option value")
			}
		}
		switch option.Name {
		case "-c", "--config", "--enable", "--disable":
			bound.process.ConfigArgs = append(bound.process.ConfigArgs, option.Name, value)
		case "-m", "--model", "-C", "--cd", "--no-alt-screen", "--no-daemon":
			// Cwd is already resolved by the supervisor. Terminal-only flags
			// stay on the original argv for the eventual interactive handoff.
		case "-s", "--sandbox":
			bound.thread.Sandbox = value
		case "-a", "--ask-for-approval":
			bound.thread.ApprovalPolicy = value
		case "--strict-config":
			bound.process.StrictConfig = true
		case "--search":
			search = true
		case "--yolo", "--dangerously-bypass-approvals-and-sandbox":
			bypass = true
		case "-p", "--profile":
			// Codex 0.157.1 runtime profiles select <name>.config.toml via a
			// loader override. app-server rejects --profile; -c profile=...
			// is not equivalent. Never silently fall back to default config.
			return nativeCodexLaunch{}, fmt.Errorf("native Codex app-server does not support CLI profiles; no context was injected")
		default:
			// Recognized but not faithfully mapped (images, provider picker,
			// extra writable roots, hook trust, auto review, etc.). Do not
			// discard even one setting in order to enable large input.
			return nativeCodexLaunch{}, fmt.Errorf("native Codex launch has an unsupported runtime option; no context was injected")
		}
	}
	if !validNativeThreadChoices(bound.thread) {
		return nativeCodexLaunch{}, fmt.Errorf("native Codex model or permission selection is invalid")
	}
	if bypass && bound.thread.ApprovalPolicy != "" {
		return nativeCodexLaunch{}, fmt.Errorf("native Codex bypass and explicit approval options conflict")
	}
	if bypass {
		// Native TUI gives the explicit bypass flag precedence independent
		// of its argv position. Do not invent this mode when it was omitted.
		bound.thread.Sandbox, bound.thread.ApprovalPolicy = "danger-full-access", "never"
	}
	if search {
		// Native TUI adds this after config overrides. Feature toggles affect
		// the separate features.* table and remain native-validated.
		bound.process.ConfigArgs = append(bound.process.ConfigArgs, "-c", `web_search="live"`)
	}
	raw, err := json.Marshal(struct {
		Executable string
		Cwd        string
		Args       []string
	}{req.Executable, req.Cwd, req.Intent.ProviderArgs})
	if err != nil {
		return nativeCodexLaunch{}, err
	}
	bound.intent = domain.HashContent(raw)
	return bound, nil
}

func validNativeThreadChoices(opts nativecodex.ThreadOptions) bool {
	if strings.ContainsAny(opts.Model, "\x00\r\n") {
		return false
	}
	if opts.Sandbox != "" && opts.Sandbox != "read-only" && opts.Sandbox != "workspace-write" && opts.Sandbox != "danger-full-access" {
		return false
	}
	return opts.ApprovalPolicy == "" || opts.ApprovalPolicy == "never" || opts.ApprovalPolicy == "untrusted" || opts.ApprovalPolicy == "on-request"
}

// start returns only a configuration-bound fresh runtime. Neither its native
// ACK nor SettingsHash establishes model/window or complete host-token evidence.
func (n nativeCodexLaunch) start(ctx context.Context, env []string) (*nativecodex.Session, nativecodex.Thread, error) {
	opts := n.process
	opts.ConfigArgs = append([]string(nil), opts.ConfigArgs...)
	if env != nil {
		opts.Env = append([]string{}, env...)
	}
	session, err := nativecodex.Start(ctx, opts)
	if err != nil {
		return nil, nativecodex.Thread{}, err
	}
	if !strings.HasPrefix(session.HostIdentity(), "cxthub_native_transport/0.157.1 ") {
		_ = session.Close()
		return nil, nativecodex.Thread{}, fmt.Errorf("native Codex launch mapping has not been verified for this host version")
	}
	thread, err := session.StartThread(ctx, n.thread)
	if err != nil {
		_ = session.Close()
		return nil, nativecodex.Thread{}, err
	}
	return session, thread, nil
}
