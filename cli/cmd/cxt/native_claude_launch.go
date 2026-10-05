package main

import (
	"fmt"
	"path/filepath"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Use the supervisor's argument classifier; never interpret settings or split
// a shell string here. Native still owns model defaults and permission rules.
type nativeClaudeLaunch struct {
	options nativeclaude.Options
	prompt  domain.AgentInitialPrompt
}

func (nativeClaudeLaunch) String() string     { return "native Claude launch (private input)" }
func (n nativeClaudeLaunch) GoString() string { return n.String() }

func bindNativeClaudeLaunch(req delivcli.ProviderLaunchRequest) (nativeClaudeLaunch, error) {
	var bound nativeClaudeLaunch
	if req.Intent.Provider != domain.ProviderClaude || req.Executable == "" || !filepath.IsAbs(req.Cwd) {
		return bound, fmt.Errorf("native Claude launch requires an executable and an absolute directory")
	}
	details, err := req.ArgumentDetails()
	if err != nil || details.Mode != "fresh interactive session" || len(details.Prompts) > 1 {
		return bound, fmt.Errorf("native Claude requires a fresh session with at most one initial question")
	}
	bound.prompt, err = req.InitialPrompt()
	if err != nil {
		return nativeClaudeLaunch{}, fmt.Errorf("native Claude initial question could not be bound")
	}
	bound.options = nativeclaude.Options{Executable: req.Executable, Cwd: req.Cwd, Model: details.Model}
	modelSeen := false
	for _, option := range details.Options {
		if option.Name == "--model" {
			if modelSeen || len(option.Values) != 1 || option.Values[0] == "" {
				return nativeClaudeLaunch{}, fmt.Errorf("native Claude model selection is ambiguous")
			}
			modelSeen = true
			continue
		}
		// A variadic option is not equivalent to repeated flags in every native
		// parser. Reject unsupported shapes instead of dropping their values.
		if len(option.Values) > 1 {
			return nativeClaudeLaunch{}, fmt.Errorf("native Claude option has an unsupported value shape")
		}
		arg := option.Name
		if canonical := map[string]string{"-n": "--name", "-d": "--debug"}[arg]; canonical != "" {
			arg = canonical
		}
		if len(option.Values) == 1 {
			arg += "=" + option.Values[0]
		}
		bound.options.ConfigArgs = append(bound.options.ConfigArgs, arg)
	}
	return bound, nil
}
