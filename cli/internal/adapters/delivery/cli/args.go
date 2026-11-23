package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type commandFlagKind uint8

const (
	commandBoolFlag commandFlagKind = iota
	commandValueFlag
)

type commandArgSpec struct {
	usage            string
	flags            map[string]commandFlagKind
	minArgs, maxArgs int
}

type parsedCommand struct {
	name        string
	positionals []string
	flags       map[string]string
}

func (p parsedCommand) first() string {
	if len(p.positionals) == 0 {
		return ""
	}
	return p.positionals[0]
}
func (p parsedCommand) last() string {
	if len(p.positionals) == 0 {
		return ""
	}
	return p.positionals[len(p.positionals)-1]
}
func (p parsedCommand) has(name string) bool {
	_, ok := p.flags[name]
	return ok
}

func commandFlags(values, bools []string) map[string]commandFlagKind {
	out := make(map[string]commandFlagKind, len(values)+len(bools))
	for _, name := range values {
		out[name] = commandValueFlag
	}
	for _, name := range bools {
		out[name] = commandBoolFlag
	}
	return out
}

// This registry owns public names, argument arity, options and initial effects.
// Subcommand-dependent constraints below operate on its typed parse, never on a
// global option dictionary or the unparsed argv.
var commandArgSpecs = map[string]commandArgSpec{
	"setup":     {usage: "cxt setup [remote-url] [--no-login]", maxArgs: 1, flags: commandFlags(nil, []string{"--no-login"})},
	"init":      {usage: "cxt init [--no-hooks] [--remote <url>]", flags: commandFlags([]string{"--remote"}, []string{"--no-hooks"})},
	"repo":      {usage: "cxt repo create <url>", minArgs: 2, maxArgs: 2},
	"remote":    {usage: "cxt remote [-v] | add <name> <url> | remove <name> | set-endpoint <name> <api-url>", maxArgs: 3, flags: commandFlags(nil, []string{"-v"})},
	"config":    {usage: "cxt config <key> [value]", minArgs: 1, maxArgs: 2},
	"login":     {usage: "cxt login [token|-t <token>] [--server <server-url>]", maxArgs: 1, flags: commandFlags([]string{"-t", "--server"}, nil)},
	"logout":    {usage: "cxt logout"},
	"segments":  {usage: segmentsUsage, maxArgs: 3, flags: commandFlags([]string{"--at", "--branch", "--to"}, []string{"--json", "--yes"})},
	"secrets":   {usage: "cxt secrets push|pull [-p <passphrase>] [--remember] [--rotate (push only)] [--force (pull only)]", minArgs: 1, maxArgs: 1, flags: commandFlags([]string{"-p"}, []string{"--remember", "--rotate", "--force"})},
	"settings":  {usage: "cxt settings pull|list|restore [n]", minArgs: 1, maxArgs: 2},
	"hooks":     {usage: "cxt hooks install|uninstall", minArgs: 1, maxArgs: 1},
	"hook":      {usage: "cxt hook --provider claude|codex --event <event>", flags: commandFlags([]string{"--provider", "--event"}, nil)},
	"version":   {usage: "cxt version"},
	"--version": {usage: "cxt --version"},
}

// segmentsUsage names every session command. Each verb is also a top-level
// command: `cxt push` is `cxt segments push`.
const segmentsUsage = "cxt segments [--json] | cxt doctor [--json] | cxt push|log [claude|codex <session>] [--json] | cxt remove claude|codex <session> --yes [--json] | cxt allow|resend claude|codex <session> [--json] | cxt archive|unarchive claude|codex <session> | cxt refresh | cxt fork [claude|codex <session>] [--at <commit>] [--branch <name>] [--to claude|codex] [--json]  (each also as cxt segments <command>)"

// PreflightArgs must run before composition. It only parses and prints help;
// invalid input cannot touch repository state, providers, or the network.
func PreflightArgs(args []string) (handled bool, err error) {
	args = CanonicalArgs(args)
	defer func() {
		if err != nil {
			err = argumentError{err}
		}
	}()
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
	if cmd == "git-hook" {
		return false, nil
	} // Versioned internal protocol owns its args.
	_, help, err := parseCommand(cmd, args[2:])
	if err != nil {
		return false, err
	}
	if help {
		return printSubcommandUsage(cmd)
	}
	return false, nil
}

func printSubcommandUsage(cmd string) (bool, error) {
	if cmd == "help" {
		printUsage()
		return true, nil
	}
	if segmentVerbs[cmd] {
		cmd = "segments"
	}
	spec, ok := commandArgSpecs[cmd]
	if !ok {
		return false, unknownCommandError(cmd)
	}
	fmt.Printf("usage: %s\n", spec.usage)
	return true, nil
}

func parseCommand(cmd string, args []string) (parsedCommand, bool, error) {
	spec, ok := commandArgSpecs[cmd]
	if !ok {
		return parsedCommand{}, false, unknownCommandError(cmd)
	}
	p := parsedCommand{name: cmd, flags: make(map[string]string)}
	literal := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if literal {
			p.positionals = append(p.positionals, arg)
			continue
		}
		if arg == "--" {
			literal = true
			continue
		}
		if arg == "-h" || arg == "--help" {
			return p, true, nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			p.positionals = append(p.positionals, arg)
			continue
		}
		name, value, inline := splitFlagValue(arg)
		kind, ok := spec.flags[name]
		if !ok {
			return p, false, fmt.Errorf("%s: unknown flag %q\nusage: %s", cmd, name, spec.usage)
		}
		key := name
		if _, exists := p.flags[key]; exists {
			return p, false, fmt.Errorf("%s: duplicate flag %q\nusage: %s", cmd, key, spec.usage)
		}
		if kind == commandBoolFlag {
			if inline {
				return p, false, fmt.Errorf("%s: flag %q does not take a value\nusage: %s", cmd, name, spec.usage)
			}
			p.flags[key] = "true"
			continue
		}
		if !inline {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			}
		}
		if value == "" {
			return p, false, fmt.Errorf("%s: flag %q requires a value\nusage: %s", cmd, name, spec.usage)
		}
		p.flags[key] = value
	}
	if err := validateCommand(cmd, &p, spec); err != nil {
		return p, false, err
	}
	return p, false, nil
}

func validateCommand(cmd string, p *parsedCommand, spec commandArgSpec) error {
	fail := func(reason string) error { return fmt.Errorf("%s: %s\nusage: %s", cmd, reason, spec.usage) }
	pos, flags := p.positionals, p.flags
	if len(pos) < spec.minArgs || len(pos) > spec.maxArgs {
		return fail("incorrect number of positional arguments")
	}
	for i, value := range pos {
		// An explicit empty config value can reset a string setting. Empty
		// targets elsewhere must not silently fall back to the current ref.
		if value == "" && !(cmd == "config" && i == 1) {
			return fail("positional arguments must not be empty")
		}
	}
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	only := func(names ...string) bool {
		for flag := range flags {
			found := false
			for _, name := range names {
				if flag == name {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	valid := true
	switch cmd {
	case "repo":
		valid = sub == "create"
	case "login":
		valid = len(pos) == 0 || flags["-t"] == ""
	case "hook":
		valid = flags["--provider"] != "" && flags["--event"] != ""
	case "hooks":
		valid = sub == "install" || sub == "uninstall"
	case "segments":
		named := len(pos) == 3 && segmentSessionKeyValid(pos[1], pos[2])
		switch sub {
		case "":
			valid = len(pos) == 0
		case "push", "log":
			// Every session, or one named by its tool and identifier.
			valid = len(pos) == 1 || named
		case "remove", "allow", "resend":
			valid = named
		case "archive", "unarchive":
			_, asJSON := flags["--json"]
			valid = named && !asJSON
		case "doctor":
			// It reads this computer and the server and changes nothing.
			valid = len(pos) == 1
		case "refresh":
			_, asJSON := flags["--json"]
			valid = len(pos) == 1 && !asJSON
		case "fork":
			// A Git branch at a commit, or a session named by its tool and
			// identifier whose conversation is continued.
			to, hasTo := flags["--to"]
			switch {
			case len(pos) == 1:
				valid = flags["--at"] != "" && flags["--branch"] != "" && !hasTo
			case named:
				_, hasBranch := flags["--branch"]
				valid = (!hasTo || to == string(domain.ProviderClaude) || to == string(domain.ProviderCodex)) && (!hasBranch || flags["--at"] != "")
			}
		default:
			valid = false
		}
		// A commit, a branch and a tool are named only for a fork.
		for _, name := range []string{"--at", "--branch", "--to"} {
			if _, given := flags[name]; given && sub != "fork" {
				valid = false
			}
		}
		// Only removing is confirmed.
		if _, yes := flags["--yes"]; yes && sub != "remove" {
			valid = false
		}
	case "config":
		valid = false
		for _, key := range []string{"capture.identity", "secrets.scrub", "secrets.redact", "secrets.minlen", "segments.auto"} {
			if sub == key {
				valid = true
			}
		}
	case "remote":
		switch sub {
		case "":
			valid = len(pos) == 0 && only("-v")
		case "add":
			valid = len(pos) == 3 && only()
		case "set-endpoint":
			valid = len(pos) == 3 && only()
		case "remove", "rm":
			valid = len(pos) == 2 && only()
		default:
			valid = false
		}
	case "secrets":
		valid = sub == "push" && only("-p", "--remember", "--rotate") || sub == "pull" && only("-p", "--remember", "--force")
	case "settings":
		switch sub {
		case "list":
			valid = len(pos) == 1
		case "pull":
			valid = len(pos) == 1
		case "restore":
			if len(pos) == 2 {
				n, err := strconv.Atoi(pos[1])
				valid = err == nil && n >= 0
			}
		default:
			valid = false
		}
	}
	if !valid {
		return fail("unsupported argument or flag combination")
	}
	return nil
}

func splitFlagValue(arg string) (name, value string, inline bool) { return strings.Cut(arg, "=") }
func unknownCommandError(cmd string) error {
	return fmt.Errorf("%q: unknown command. Supported: %s", cmd, strings.Join(publicCommandNames, "|"))
}
func publicCommands() []string {
	names := []string{"help"}
	for name := range commandArgSpecs {
		if !strings.HasPrefix(name, "-") {
			names = append(names, name)
		}
	}
	for name := range segmentVerbs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// flagConsumesValue reports whether a flag of any command takes a value.
func flagConsumesValue(name string) bool {
	name, _, _ = splitFlagValue(name)
	for _, spec := range commandArgSpecs {
		if spec.flags[name] == commandValueFlag {
			return true
		}
	}
	return false
}

// segmentSessionKeyValid reports whether a tool and a session identifier name
// a session the way the server and the .cxt folder key it.
func segmentSessionKeyValid(provider, session string) bool {
	return (provider == string(domain.ProviderClaude) || provider == string(domain.ProviderCodex)) && domain.ValidAnchoredSessionID(session)
}
