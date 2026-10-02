package cli

import (
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type providerLaunchMode string

const (
	providerFresh          providerLaunchMode = "fresh interactive session"
	providerNative         providerLaunchMode = "native resume/continue"
	providerDiagnostic     providerLaunchMode = "help/version"
	providerNoninteractive providerLaunchMode = "noninteractive or provider management command"
	providerUnmanaged      providerLaunchMode = "provider-managed remote/worktree/session selection"
)

type providerInvocation struct {
	Mode            providerLaunchMode
	SessionID       string
	RestartArgs     []string
	Directory       string
	Model           string
	Profile         string
	ConfigOverrides []string
	Prompts         []string
	Options         []ProviderLaunchOption
}

// ProviderLaunchOption is a recognized option, normalized by the single
// invocation parser. Values remain private launch input, never receipt data.
type ProviderLaunchOption struct {
	Name   string
	Values []string
}

// ProviderLaunchDetails exposes parsed arguments to composition without a
// second flag parser. Model is only an explicit --model/-m value; profile and
// config overrides still need resolution by the host capability adapter.
type ProviderLaunchDetails struct {
	Mode            string
	Model           string
	Profile         string
	ConfigOverrides []string
	Prompts         []string
	Directory       string
	SessionID       string
	Options         []ProviderLaunchOption
}

func (r ProviderLaunchRequest) ArgumentDetails() (ProviderLaunchDetails, error) {
	inv, err := inspectLaunchIntent(r.Intent)
	if err != nil {
		return ProviderLaunchDetails{}, err
	}
	options := make([]ProviderLaunchOption, len(inv.Options))
	for i, option := range inv.Options {
		options[i] = ProviderLaunchOption{Name: option.Name, Values: append([]string(nil), option.Values...)}
	}
	return ProviderLaunchDetails{Mode: string(inv.Mode), Model: inv.Model, Profile: inv.Profile,
		ConfigOverrides: append([]string(nil), inv.ConfigOverrides...), Prompts: append([]string(nil), inv.Prompts...),
		Directory: inv.Directory, SessionID: inv.SessionID, Options: options}, nil
}

type providerOption struct {
	values  int // 0 flag, 1 required, -1 optional, 2 one or more
	mode    providerLaunchMode
	restart bool
	session bool
	dir     bool
}

// These argument shapes were checked against codex-cli 0.157.1 and Claude
// Code 2.1.284. This is an intent classifier, not a replacement provider parser.
// Unknown options fail before preparation instead of guessing whether the next
// token is a prompt, a model value, or a native resume command.
func providerOptions(provider domain.ProviderKind) map[string]providerOption {
	out := map[string]providerOption{}
	add := func(names string, option providerOption) {
		for _, name := range strings.Fields(names) {
			out[name] = option
		}
	}
	value := providerOption{values: 1, restart: true}
	flag := providerOption{restart: true}
	add("--help -h", providerOption{mode: providerDiagnostic})
	if provider == domain.ProviderCodex {
		add("--version -V", providerOption{mode: providerDiagnostic})
		add("-c --config --enable --disable -m --model -p --profile -s --sandbox -a --ask-for-approval --local-provider --add-dir", value)
		add("--oss --strict-config --approve-for-me --dangerously-bypass-approvals-and-sandbox --yolo --dangerously-bypass-hook-trust --search --no-alt-screen --no-daemon --full-auto", flag)
		add("-C --cd", providerOption{values: 1, restart: true, dir: true})
		add("-i --image", providerOption{values: 2})
		add("--last --all --include-non-interactive", providerOption{mode: providerNative})
		add("--remote --remote-auth-token-env", providerOption{values: 1, mode: providerUnmanaged})
		add("--worktree", providerOption{mode: providerUnmanaged})
		return out
	}
	add("--version -v", providerOption{mode: providerDiagnostic})
	add("--resume -r --from-pr --teleport", providerOption{values: -1, mode: providerNative, session: true})
	add("--continue -c --fork-session", providerOption{mode: providerNative})
	add("--print -p", providerOption{mode: providerNoninteractive})
	add("--cloud --remote-control --background --bg", providerOption{values: -1, mode: providerUnmanaged})
	add("--worktree -w", providerOption{values: -1, mode: providerUnmanaged})
	add("--session-id --environment", providerOption{values: 1, mode: providerUnmanaged})
	add("--agent --agents --append-system-prompt --append-system-prompt-file --system-prompt --system-prompt-file --debug-file --effort --fallback-model --input-format --json-schema --max-budget-usd --model -n --name --output-format --permission-mode --permission-prompts --plugin-dir --plugin-url --remote-control-session-name-prefix --setting-sources --settings --system-prompt-snapshot --client-data-url", value)
	add("--add-dir --allowedTools --allowed-tools --disallowedTools --disallowed-tools --betas --file --mcp-config --tools", providerOption{values: 2, restart: true})
	add("--debug -d --prompt-suggestions --tmux", providerOption{values: -1, restart: true})
	add("--bare --brief --chrome --no-chrome --dangerously-skip-permissions --allow-dangerously-skip-permissions --disable-slash-commands --exclude-dynamic-system-prompt-sections --forward-subagent-text --ide --include-hook-events --include-partial-messages --no-session-persistence --replay-user-messages --restricted --safe-mode --strict-mcp-config --verbose", flag)
	return out
}

func providerCommandMode(provider domain.ProviderKind, arg string) (providerLaunchMode, bool) {
	if provider == domain.ProviderCodex {
		switch arg {
		case "resume", "fork":
			return providerNative, true
		case "help", "version":
			return providerDiagnostic, true
		case "agents", "exec", "e", "review", "login", "logout", "mcp", "plugin", "app-server", "remote-control", "app", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "cloud", "cloud-tasks", "exec-server", "features", "execpolicy":
			return providerNoninteractive, true
		}
	} else {
		switch arg {
		case "help", "version":
			return providerDiagnostic, true
		case "attach", "agents", "auth", "auto-mode", "doctor", "gateway", "import", "install", "logs", "mcp", "plugin", "plugins", "project", "respawn", "rm", "setup-token", "stop", "kill", "ultrareview", "update", "upgrade":
			return providerNoninteractive, true
		}
	}
	return "", false
}

func inspectProviderInvocation(provider domain.ProviderKind, args []string) (providerInvocation, error) {
	inv := providerInvocation{Mode: providerFresh}
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return inv, fmt.Errorf("%s argument contains a NUL byte", provider)
		}
	}
	options := providerOptions(provider)
	literal, firstPositional, codexResume, resumeTargetSeen := false, true, false, false
	setMode := func(mode providerLaunchMode) {
		// Diagnostic and noninteractive requests never prepare a package even
		// when they also mention a native session (e.g. exec resume / -p -r).
		if mode == "" || inv.Mode == providerDiagnostic {
			return
		}
		if mode == providerDiagnostic || inv.Mode == providerFresh || mode == providerNoninteractive || (mode == providerUnmanaged && inv.Mode == providerNative) {
			inv.Mode = mode
		}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && !literal {
			literal = true
			continue
		}
		if !literal && strings.HasPrefix(arg, "-") && arg != "-" {
			name, inlineValue, inline := strings.Cut(arg, "=")
			option, ok := options[name]
			// Providers accept attached values such as -mMODEL and -rUUID.
			if !ok && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
				if short, found := options[arg[:2]]; found && short.values != 0 {
					name, option, ok = arg[:2], short, true
					inlineValue, inline = arg[2:], true
					inlineValue = strings.TrimPrefix(inlineValue, "=")
				}
			}
			if !ok {
				return inv, fmt.Errorf("cannot safely classify %s option %q; use the provider directly for unsupported launch modes", provider, name)
			}
			start := i
			value := inlineValue
			if inline && option.values == 0 {
				return inv, fmt.Errorf("%s option %s does not accept a value", provider, name)
			}
			if !inline && option.values != 0 && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
				if option.values == 2 {
					for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						i++
					}
				}
			} else if !inline && option.values > 0 {
				return inv, fmt.Errorf("%s option %s requires a value", provider, name)
			}
			setMode(option.mode)
			parsed := ProviderLaunchOption{Name: name}
			if inline {
				parsed.Values = []string{value}
			} else if i > start {
				parsed.Values = append([]string(nil), args[start+1:i+1]...)
			}
			inv.Options = append(inv.Options, parsed)
			if option.dir {
				inv.Directory = value
			}
			if name == "--model" || provider == domain.ProviderCodex && name == "-m" {
				inv.Model = value
			}
			if provider == domain.ProviderCodex {
				switch name {
				case "-p", "--profile":
					inv.Profile = value
				case "-c", "--config":
					inv.ConfigOverrides = append(inv.ConfigOverrides, value)
				}
			}
			if option.session && (name == "--resume" || name == "-r") && providerfs.ValidSessionID(value) {
				inv.SessionID = value
			}
			if option.restart {
				inv.RestartArgs = append(inv.RestartArgs, args[start:i+1]...)
			}
			continue
		}
		if firstPositional && !literal {
			firstPositional = false
			if mode, ok := providerCommandMode(provider, arg); ok {
				setMode(mode)
				if mode == providerDiagnostic || mode == providerNoninteractive || mode == providerUnmanaged {
					// Their remaining arguments have a different grammar. Leave
					// validation to that provider command and never inject.
					return inv, nil
				}
				codexResume = provider == domain.ProviderCodex && arg == "resume"
				continue
			}
		}
		firstPositional = false
		if codexResume && !resumeTargetSeen {
			resumeTargetSeen = true
			if providerfs.ValidSessionID(arg) {
				inv.SessionID = arg
			}
			continue
		}
		inv.Prompts = append(inv.Prompts, arg)
	}
	return inv, nil
}
