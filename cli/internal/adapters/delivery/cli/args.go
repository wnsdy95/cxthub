package cli

import (
	"fmt"
	"strings"
)

type commandFlagKind uint8

const (
	commandBoolFlag commandFlagKind = iota
	commandValueFlag
)

type commandArgSpec struct {
	usage       string
	flags       map[string]commandFlagKind
	passthrough bool
}

func commandFlags(valueFlags, boolFlags []string) map[string]commandFlagKind {
	out := make(map[string]commandFlagKind, len(valueFlags)+len(boolFlags))
	for _, name := range valueFlags {
		out[name] = commandValueFlag
	}
	for _, name := range boolFlags {
		out[name] = commandBoolFlag
	}
	return out
}

var commandArgSpecs = map[string]commandArgSpec{
	"setup":     {usage: "cxt setup [remote-url] [--no-login]", flags: commandFlags(nil, []string{"--no-login"})},
	"init":      {usage: "cxt init [--no-hooks] [--remote <url>]", flags: commandFlags([]string{"--remote"}, []string{"--no-hooks"})},
	"repo":      {usage: "cxt repo create <url>"},
	"claude":    {usage: "cxt claude [claude-arguments...]", passthrough: true},
	"codex":     {usage: "cxt codex [codex-arguments...]", passthrough: true},
	"remote":    {usage: "cxt remote [-v] | add <name> <url> | remove <name>", flags: commandFlags(nil, []string{"-v"})},
	"repack":    {usage: "cxt repack"},
	"add":       {usage: "cxt add [claude|codex|.]..."},
	"commit":    {usage: "cxt commit [-m <message>]", flags: commandFlags([]string{"-m"}, nil)},
	"switch":    {usage: "cxt switch [<branch>] [-c <new>] [--mode full|reconstructed|memory]", flags: commandFlags([]string{"-c", "--mode"}, nil)},
	"config":    {usage: "cxt config <key> [value]"},
	"login":     {usage: "cxt login [token] | -t <token>", flags: commandFlags([]string{"-t"}, nil)},
	"logout":    {usage: "cxt logout"},
	"fsck":      {usage: "cxt fsck"},
	"reflog":    {usage: "cxt reflog"},
	"secrets":   {usage: "cxt secrets push|pull [-p <passphrase>] [--remember] [--rotate]", flags: commandFlags([]string{"-p"}, []string{"--remember", "--rotate"})},
	"settings":  {usage: "cxt settings pull|list|restore [n]"},
	"hooks":     {usage: "cxt hooks install|uninstall"},
	"save":      {usage: "cxt save [-m <message>] [--provider claude|codex]", flags: commandFlags([]string{"-m", "--provider"}, nil)},
	"list":      {usage: "cxt list [--branch <branch>]", flags: commandFlags([]string{"--branch"}, nil)},
	"log":       {usage: "cxt log [--branch <branch>]", flags: commandFlags([]string{"--branch"}, nil)},
	"checkout":  {usage: "cxt checkout [<ref>] [-b <new>] [--provider claude|codex] [--mode full|reconstructed|memory]", flags: commandFlags([]string{"-b", "--provider", "--mode"}, nil)},
	"fork":      {usage: "cxt fork <ref> --as <branch> [--provider claude|codex] [--mode full|reconstructed|memory]", flags: commandFlags([]string{"--as", "--provider", "--mode"}, nil)},
	"load":      {usage: "cxt load [<ref>] [--provider claude|codex] [--mode full|reconstructed|memory]", flags: commandFlags([]string{"--provider", "--mode"}, nil)},
	"push":      {usage: "cxt push [--force|-f|--append]", flags: commandFlags(nil, []string{"--force", "-f", "--append"})},
	"pull":      {usage: "cxt pull [--force|-f]", flags: commandFlags(nil, []string{"--force", "-f"})},
	"stash":     {usage: "cxt stash [push [-m <message>]|pop|list]", flags: commandFlags([]string{"-m"}, nil)},
	"memorize":  {usage: "cxt memorize [<ref>] [--provider claude|codex]", flags: commandFlags([]string{"--provider"}, nil)},
	"memory":    {usage: "cxt memory [<ref>] [--provider claude|codex]", flags: commandFlags([]string{"--provider"}, nil)},
	"tag":       {usage: "cxt tag [<name> [ref]]"},
	"mcp":       {usage: "cxt mcp"},
	"hook":      {usage: "cxt hook --provider claude|codex --event <event>", flags: commandFlags([]string{"--provider", "--event"}, nil)},
	"version":   {usage: "cxt version"},
	"--version": {usage: "cxt --version"},
}

// PreflightArgs handles help and validates flags before the composition root
// creates adapters or any command can touch filesystem, network, refs, or
// provider sessions. The boolean reports that help was printed and execution
// must stop successfully.
func PreflightArgs(args []string) (bool, error) {
	if len(args) < 2 {
		printUsage()
		return true, nil
	}
	cmd := args[1]
	if cmd == "-h" || cmd == "--help" {
		printUsage()
		return true, nil
	}
	if cmd == "help" {
		if len(args) == 2 {
			printUsage()
			return true, nil
		}
		if len(args) != 3 {
			return false, fmt.Errorf("usage: cxt help [command]")
		}
		return printSubcommandUsage(args[2])
	}
	if cmd == "git-hook" { // internal Git integration owns its event-specific arguments
		return false, nil
	}

	spec, ok := commandArgSpecs[cmd]
	if !ok {
		return false, unknownCommandError(cmd)
	}
	rest := args[2:]
	if helpRequested(rest) {
		fmt.Printf("usage: %s\n", spec.usage)
		return true, nil
	}
	if spec.passthrough {
		return false, nil
	}
	if err := validateCommandFlags(cmd, rest, spec); err != nil {
		return false, err
	}
	return false, nil
}

func printSubcommandUsage(cmd string) (bool, error) {
	if cmd == "help" {
		printUsage()
		return true, nil
	}
	spec, ok := commandArgSpecs[cmd]
	if !ok {
		return false, unknownCommandError(cmd)
	}
	fmt.Printf("usage: %s\n", spec.usage)
	return true, nil
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func validateCommandFlags(cmd string, args []string, spec commandArgSpec) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, inlineValue, hasInlineValue := splitFlagValue(arg)
		kind, ok := spec.flags[name]
		if !ok {
			return fmt.Errorf("%s: unknown flag %q\nusage: %s", cmd, arg, spec.usage)
		}
		if hasInlineValue {
			if kind != commandValueFlag {
				return fmt.Errorf("%s: flag %q does not take a value\nusage: %s", cmd, name, spec.usage)
			}
			if inlineValue == "" {
				return fmt.Errorf("%s: flag %q requires a value\nusage: %s", cmd, name, spec.usage)
			}
			continue
		}
		if kind != commandValueFlag {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return fmt.Errorf("%s: flag %q requires a value\nusage: %s", cmd, name, spec.usage)
		}
		i++
	}

	pos := positionals(args)
	switch cmd {
	case "remote":
		if flagPresent(args, "-v") && len(pos) > 0 {
			return fmt.Errorf("remote: flag %q is only valid when listing remotes\nusage: %s", "-v", spec.usage)
		}
	case "secrets":
		if flagPresent(args, "--rotate") && (len(pos) == 0 || pos[0] != "push") {
			return fmt.Errorf("secrets: flag %q is only valid with push\nusage: %s", "--rotate", spec.usage)
		}
	case "stash":
		if flagVal(args, "-m") != "" && len(pos) > 0 && pos[0] != "push" {
			return fmt.Errorf("stash: flag %q is only valid with push\nusage: %s", "-m", spec.usage)
		}
	}
	return nil
}

func splitFlagValue(arg string) (name, value string, inline bool) {
	name, value, inline = strings.Cut(arg, "=")
	return name, value, inline
}

func unknownCommandError(cmd string) error {
	return fmt.Errorf("%q: unknown command. Supported: %s", cmd, strings.Join(publicCommandNames, "|"))
}

func flagConsumesValue(name string) bool {
	name, _, _ = splitFlagValue(name)
	for _, spec := range commandArgSpecs {
		if spec.flags[name] == commandValueFlag {
			return true
		}
	}
	return false
}

func providerPassthroughArgs(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}
