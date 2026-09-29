package cli

import (
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MaxLaunchContextBudget bounds the entire CXT-supplied package, not the
// provider's model window. Host capacity must be checked separately.
const MaxLaunchContextBudget = domain.MaxAgentContextTokens

// LaunchIntent describes one invocation. ProviderArgs are an owned, unchanged
// copy of everything after the provider name, including a literal --.
type LaunchIntent struct {
	Provider      domain.ProviderKind
	ProviderArgs  []string
	Pull          bool
	ContextBudget int
	WorkStatePath string
}

// ParseLaunchIntent accepts arguments after the cxt executable. recognized is
// false for ordinary CXT commands. CXT prefix options end at the provider name;
// options after that boundary always belong to the provider.
func ParseLaunchIntent(args []string) (intent LaunchIntent, recognized bool, err error) {
	if len(args) == 0 {
		return intent, false, nil
	}
	switch args[0] {
	case "codex", "claude", "--pull", "--context-budget", "--work-state", "--":
		recognized = true
	default:
		if !strings.HasPrefix(args[0], "--context-budget=") && !strings.HasPrefix(args[0], "--pull=") && !strings.HasPrefix(args[0], "--work-state=") {
			return intent, false, nil
		}
		recognized = true
	}
	seenPull, seenBudget, seenWork, literal := false, false, false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "codex" || arg == "claude" {
			intent.Provider = domain.ProviderKind(arg)
			intent.ProviderArgs = append([]string(nil), args[i+1:]...)
			if seenBudget && !seenPull {
				return intent, true, fmt.Errorf("--context-budget requires prefix --pull")
			}
			if seenPull && !seenBudget {
				intent.ContextBudget = MaxLaunchContextBudget
			}
			if _, err := inspectLaunchIntent(intent); err != nil {
				return intent, true, err
			}
			return intent, true, nil
		}
		if literal {
			return intent, true, fmt.Errorf("expected codex or claude after --, got %q", arg)
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--work-state":
			if seenWork {
				return intent, true, fmt.Errorf("duplicate --work-state")
			}
			seenWork = true
			if !inline {
				if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
					return intent, true, fmt.Errorf("--work-state requires a file")
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return intent, true, fmt.Errorf("--work-state requires a file")
			}
			intent.WorkStatePath = value
		case "--":
			literal = true
		case "--pull":
			if seenPull || inline {
				return intent, true, fmt.Errorf("prefix --pull must appear once and takes no value")
			}
			seenPull, intent.Pull = true, true
		case "--context-budget":
			if seenBudget {
				return intent, true, fmt.Errorf("duplicate --context-budget")
			}
			seenBudget = true
			if !inline {
				if i+1 == len(args) {
					return intent, true, fmt.Errorf("--context-budget requires a token count or full")
				}
				i++
				value = args[i]
			}
			intent.ContextBudget, err = ParseLaunchContextBudget(value)
			if err != nil {
				return intent, true, err
			}
		default:
			return intent, true, fmt.Errorf("unsupported CXT launch prefix %q; put provider arguments after codex or claude", arg)
		}
	}
	return intent, true, fmt.Errorf("CXT launch prefix requires codex or claude")
}

func ParseLaunchContextBudget(value string) (int, error) {
	// An omitted prefix budget is full; an explicitly supplied empty value is
	// a usage error, not an implicit override of the user's launch policy.
	if value == "" {
		return 0, fmt.Errorf("--context-budget requires a token count or full")
	}
	return domain.ParseHistoryBudget(value)
}

func inspectLaunchIntent(intent LaunchIntent) (providerInvocation, error) {
	if intent.Provider != domain.ProviderCodex && intent.Provider != domain.ProviderClaude {
		return providerInvocation{}, fmt.Errorf("unsupported provider %q", intent.Provider)
	}
	if intent.Pull {
		if intent.ContextBudget <= 0 || intent.ContextBudget > MaxLaunchContextBudget {
			return providerInvocation{}, fmt.Errorf("history context budget must be between 1 and %d tokens", MaxLaunchContextBudget)
		}
	} else if intent.ContextBudget != 0 {
		return providerInvocation{}, fmt.Errorf("--context-budget requires prefix --pull")
	}
	inv, err := inspectProviderInvocation(intent.Provider, intent.ProviderArgs)
	if err != nil {
		return inv, err
	}
	if inv.Mode == providerUnmanaged {
		return inv, fmt.Errorf("CXT cannot verify context selection for %s; run %s directly for this provider-managed mode", inv.Mode, intent.Provider)
	}
	if intent.Pull && inv.Mode != providerFresh {
		return inv, fmt.Errorf("prefix --pull requires a supported fresh interactive %s launch; cannot combine history with %s", intent.Provider, inv.Mode)
	}
	if intent.WorkStatePath != "" && inv.Mode != providerFresh {
		return inv, fmt.Errorf("prefix --work-state requires a supported fresh interactive %s launch; cannot combine personal handoff with %s", intent.Provider, inv.Mode)
	}
	return inv, nil
}
