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

// nativeCodexLaunch owns parsed intent, not a capacity attestation. The public
// delayed path separately requires its owned window binding and first-turn gate.
// Config and the initial task may contain secrets: keep them out of receipts,
// JSON, and routine formatting. The prompt must enter later host accounting;
// creating this fresh runtime never submits it or starts a model turn.
type nativeCodexLaunch struct {
	process     nativecodex.Options
	thread      nativecodex.ThreadOptions
	prompt      domain.AgentInitialPrompt
	intent      domain.ContentHash
	tuiRootArgs []string
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
	// Share the pre-count provider normalization and presence semantics with
	// preparation, so a later native submission uses the reserved text.
	bound.prompt, err = req.InitialPrompt()
	if err != nil {
		return nativeCodexLaunch{}, fmt.Errorf("native Codex initial prompt could not be bound")
	}
	search, bypass := false, false
	seen := map[string]bool{}
	for _, option := range details.Options {
		value := ""
		if len(option.Values) == 1 {
			value = option.Values[0]
		}
		canonical := map[string]string{"-c": "--config", "-m": "--model", "-C": "--cd", "-s": "--sandbox", "-a": "--ask-for-approval", "-p": "--profile", "--yolo": "--dangerously-bypass-approvals-and-sandbox"}[option.Name]
		if canonical == "" {
			canonical = option.Name
		}
		repeatable := canonical == "--config" || canonical == "--enable" || canonical == "--disable"
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
			// Model is already bound above; cwd comes from the native ACK.
			// The private handoff consumes --no-daemon, which conflicts with
			// --remote. Preserve the user's alternate-screen choice below.
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
		switch canonical {
		case "--cd", "--no-daemon", "--search":
			// Cwd is replaced at handoff; search gets the same final explicit
			// override as the preparation process, after all original config.
		case "--sandbox", "--ask-for-approval", "--dangerously-bypass-approvals-and-sandbox":
			// Codex 0.157.1 rejects permission flags on remote resume. These
			// choices are applied by the owned thread/start, not re-applied
			// by the TUI; the handoff response settings hash enforces equality.
		default:
			arg := canonical
			if len(option.Values) == 1 {
				// Preserve attached values beginning with '-' as values. Never
				// shell-quote, parse TOML, or reorder native feature toggles.
				arg += "=" + value
			}
			bound.tuiRootArgs = append(bound.tuiRootArgs, arg)
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
		bound.tuiRootArgs = append(bound.tuiRootArgs, `--config=web_search="live"`)
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

// tuiResumeArgs only maps bound intent. The caller must supply the endpoint
// from this session's owned private handoff and its acknowledged fresh thread;
// syntax checks cannot establish that ownership. That private, invocation-lived
// server is why --no-daemon can be consumed instead of forwarded with --remote.
// Permission flags are likewise consumed: the owned thread/start already applied
// them, and the handoff must verify unchanged settings on the resume response.
// Remote resume does not support re-applying sandbox/approval/bypass flags.
// Thread.Cwd is the canonical directory acknowledged by the native process, so
// the mapper does not resolve paths or consult mutable filesystem/config state.
// No prompt is included or released here. Capacity, source/config revalidation,
// initial-prompt accounting and environment selection remain caller obligations.
func (n nativeCodexLaunch) tuiResumeArgs(endpoint string, thread nativecodex.Thread) ([]string, error) {
	if n.intent == "" || n.process.Executable == "" || !filepath.IsAbs(n.process.Cwd) {
		return nil, fmt.Errorf("native Codex handoff requires a bound launch")
	}
	socketPath, unix := strings.CutPrefix(endpoint, "unix://")
	if !unix || !strings.HasPrefix(socketPath, "/") || !filepath.IsAbs(socketPath) ||
		socketPath == "/" || filepath.Clean(socketPath) != socketPath ||
		strings.ContainsAny(socketPath, "?#%\\") || strings.IndexFunc(socketPath, func(r rune) bool { return r < ' ' || r == '\x7f' }) >= 0 {
		// Native treats the suffix as a literal path, not a URL-escaped path.
		// Reject authority/query/fragment/escape forms and the daemon default.
		return nil, fmt.Errorf("native Codex handoff requires an absolute private Unix endpoint")
	}
	if !domain.ValidSessionID(thread.ID) || !filepath.IsAbs(thread.Cwd) ||
		filepath.Clean(thread.Cwd) != thread.Cwd || strings.ContainsAny(thread.Cwd, "\x00\r\n") {
		return nil, fmt.Errorf("native Codex handoff requires a valid fresh thread identity and canonical directory")
	}
	args := make([]string, 0, len(n.tuiRootArgs)+5)
	args = append(args, "--remote", endpoint)
	args = append(args, n.tuiRootArgs...)
	args = append(args, "--cd="+thread.Cwd, "resume", thread.ID)
	return args, nil
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
	if !nativecodex.SupportedHostIdentity(session.HostIdentity()) {
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
