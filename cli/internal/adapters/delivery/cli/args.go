package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type commandFlagKind uint8

const (
	commandBoolFlag commandFlagKind = iota
	commandValueFlag
)

// Effects describe the command's own work, not automatic permission to replay
// unrelated operations. Only context writes and explicit sync replay the journal.
type commandEffect uint8

const (
	commandRead commandEffect = iota
	commandLocalWrite
	commandContextWrite
	commandSync
	commandProvider
)

type commandArgSpec struct {
	usage            string
	flags            map[string]commandFlagKind
	passthrough      bool
	minArgs, maxArgs int // -1 permits multiple positionals (add only).
	effect           commandEffect
}

type parsedCommand struct {
	name        string
	positionals []string
	flags       map[string]string
	effect      commandEffect
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
	if name == "-f" {
		name = "--force"
	}
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
	"setup":     {usage: "cxt setup [remote-url] [--no-login]", maxArgs: 1, effect: commandLocalWrite, flags: commandFlags(nil, []string{"--no-login"})},
	"init":      {usage: "cxt init [--no-hooks] [--remote <url>]", effect: commandLocalWrite, flags: commandFlags([]string{"--remote"}, []string{"--no-hooks"})},
	"repo":      {usage: "cxt repo create <url>", minArgs: 2, maxArgs: 2, effect: commandLocalWrite},
	"claude":    {usage: "cxt claude [claude-arguments...]", passthrough: true, effect: commandProvider},
	"codex":     {usage: "cxt codex [codex-arguments...]", passthrough: true, effect: commandProvider},
	"remote":    {usage: "cxt remote [-v] | add <name> <url> | remove <name>", maxArgs: 3, flags: commandFlags(nil, []string{"-v"})},
	"branch":    {usage: "cxt branch [list] | operations [--json] | replay | recover <operation-id> --confirm-orphan | archive <name> | restore <name> [--provider claude|codex] [--mode full|reconstructed|memory]", maxArgs: 2, flags: commandFlags([]string{"--provider", "--mode"}, []string{"--json", "--confirm-orphan"})},
	"repack":    {usage: "cxt repack", effect: commandLocalWrite},
	"add":       {usage: "cxt add [claude|codex|.]... [--expect <index-revision>] [--json]", maxArgs: -1, effect: commandContextWrite, flags: commandFlags([]string{"--expect"}, []string{"--json"})},
	"commit":    {usage: "cxt commit [-m <message>] [--expect <index-revision>] [--json] | --resume <operation-id> [--json]", effect: commandContextWrite, flags: commandFlags([]string{"-m", "--expect", "--resume"}, []string{"--json"})},
	"restore":   {usage: "cxt restore --staged <source-key>...|. [--expect <index-revision>] [--json]", minArgs: 1, maxArgs: -1, effect: commandLocalWrite, flags: commandFlags([]string{"--expect"}, []string{"--staged", "--json"})},
	"switch":    {usage: "cxt switch [<branch>] [-c <new>] [--mode full|reconstructed|memory]", maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"-c", "--mode"}, nil)},
	"config":    {usage: "cxt config <key> [value]", minArgs: 1, maxArgs: 2},
	"login":     {usage: "cxt login [token|-t <token>] [--server <server-url>]", maxArgs: 1, effect: commandLocalWrite, flags: commandFlags([]string{"-t", "--server"}, nil)},
	"logout":    {usage: "cxt logout", effect: commandLocalWrite},
	"fsck":      {usage: "cxt fsck"},
	"repair":    {usage: "cxt repair --from-server [--remote <repository-url>] | --preview --ref <branch>|--snapshot <hash> --reason <text> --output <file> | --apply <file> --expect <plan-id>", effect: commandLocalWrite, flags: commandFlags([]string{"--remote", "--ref", "--snapshot", "--reason", "--output", "--apply", "--expect"}, []string{"--from-server", "--preview"})},
	"capture":   {usage: "cxt capture list [--all] [--json] | show <id> [--json] | retry|resolve <id> --expect <hash> | acknowledge <id> --expect <hash> --reason <text>", minArgs: 1, maxArgs: 2, flags: commandFlags([]string{"--expect", "--reason"}, []string{"--all", "--json"})},
	"sync":      {usage: "cxt sync status [--json]", minArgs: 1, maxArgs: 1, flags: commandFlags(nil, []string{"--json"})},
	"doctor":    {usage: "cxt doctor [--json]", flags: commandFlags(nil, []string{"--json"})},
	"status":    {usage: "cxt status [--json]", flags: commandFlags(nil, []string{"--json"})},
	"diff":      {usage: "cxt diff [--staged] [--json]", flags: commandFlags(nil, []string{"--staged", "--json"})},
	"reflog":    {usage: "cxt reflog"},
	"secrets":   {usage: "cxt secrets push|pull [-p <passphrase>] [--remember] [--rotate (push only)] [--force (pull only)]", minArgs: 1, maxArgs: 1, effect: commandLocalWrite, flags: commandFlags([]string{"-p"}, []string{"--remember", "--rotate", "--force"})},
	"settings":  {usage: "cxt settings pull|list|restore [n]", minArgs: 1, maxArgs: 2},
	"hooks":     {usage: "cxt hooks install|uninstall", minArgs: 1, maxArgs: 1, effect: commandLocalWrite},
	"save":      {usage: "cxt save [-m <message>] [--provider claude|codex]", effect: commandContextWrite, flags: commandFlags([]string{"-m", "--provider"}, nil)},
	"list":      {usage: "cxt list [<ref>|--branch <branch>|--all|--retained] [--server] [--json]", maxArgs: 1, flags: commandFlags([]string{"--branch"}, []string{"--all", "--retained", "--server", "--json"})},
	"log":       {usage: "cxt log [<ref>|--branch <branch>|--all|--retained] [--server] [--json]", maxArgs: 1, flags: commandFlags([]string{"--branch"}, []string{"--all", "--retained", "--server", "--json"})},
	"checkout":  {usage: "cxt checkout [<ref>] [-b <new>] [--provider claude|codex] [--mode full|reconstructed|memory]", maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"-b", "--provider", "--mode"}, nil)},
	"fork":      {usage: "cxt fork <ref> --as <branch> [--provider claude|codex] [--mode full|reconstructed|memory]", minArgs: 1, maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"--as", "--provider", "--mode"}, nil)},
	"load":      {usage: "cxt load [<ref>] [--work-state <file>] [--provider claude|codex] [--mode full|reconstructed|memory] | [--context-budget 200k|full] --output <file> [--model <model>]", maxArgs: 1, effect: commandProvider, flags: commandFlags([]string{"--provider", "--mode", "--context-budget", "--output", "--model", "--work-state"}, nil)},
	"push":      {maxArgs: 2, usage: "cxt push [remote [branch]] [--force|-f|--append] [--wait-history]", effect: commandSync, flags: commandFlags(nil, []string{"--force", "-f", "--append", "--wait-history"})},
	"pull":      {maxArgs: 2, usage: "cxt pull [remote [branch]]", effect: commandSync, flags: commandFlags(nil, []string{"--force", "-f"})},
	"fetch":     {maxArgs: 2, usage: "cxt fetch [remote [branch]]", effect: commandLocalWrite},
	"stash":     {usage: "cxt stash [push [-m <message>] [--provider claude|codex]|pop|list] | push|pop|list --staged [--expect <index-revision>] [--id <stash-id>] [--json]", maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"-m", "--provider", "--expect", "--id"}, []string{"--staged", "--json"})},
	"memorize":  {usage: "cxt memorize [<ref>] [--provider claude|codex] [--claims <json-file>]", maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"--provider", "--claims"}, nil)},
	"memory":    {usage: "cxt memory [<ref>] [--provider claude|codex] [--claims <json-file>]", maxArgs: 1, effect: commandContextWrite, flags: commandFlags([]string{"--provider", "--claims"}, nil)},
	"tag":       {usage: "cxt tag [<name> [ref]]", maxArgs: 2},
	"mcp":       {usage: "cxt mcp --local", flags: commandFlags(nil, []string{"--local"})},
	"hook":      {usage: "cxt hook --provider claude|codex --event <event>", effect: commandLocalWrite, flags: commandFlags([]string{"--provider", "--event"}, nil)},
	"version":   {usage: "cxt version"},
	"--version": {usage: "cxt --version"},
}

// PreflightArgs must run before composition. It only parses and prints help;
// invalid input cannot touch repository state, providers, or the network.
func PreflightArgs(args []string) (handled bool, err error) {
	defer func() {
		if err != nil {
			err = argumentError{err}
		}
	}()
	if len(args) < 2 {
		printUsage()
		return true, nil
	}
	if _, recognized, err := ParseLaunchIntent(args[1:]); recognized {
		return false, err
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
	p := parsedCommand{name: cmd, flags: make(map[string]string), effect: spec.effect}
	if spec.passthrough {
		return p, helpRequested(args), nil
	}
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
		if name == "-f" {
			key = "--force"
		}
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
	if len(pos) < spec.minArgs || spec.maxArgs >= 0 && len(pos) > spec.maxArgs {
		return fail("incorrect number of positional arguments")
	}
	for i, value := range pos {
		// An explicit empty config value can reset a string setting. Empty
		// targets elsewhere must not silently fall back to the current ref.
		if value == "" && !(cmd == "config" && i == 1) {
			return fail("positional arguments must not be empty")
		}
	}
	for _, flag := range []string{"--provider", "--mode"} {
		if value := flags[flag]; value != "" {
			valid := flag == "--provider" && (value == "claude" || value == "codex") || flag == "--mode" && (value == "full" || value == "reconstructed" || value == "memory")
			if !valid {
				return fail("invalid value for " + flag)
			}
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
	if (cmd == "add" || cmd == "commit" || cmd == "restore" || cmd == "stash") && p.has("--expect") && domain.ValidateContentHash(domain.ContentHash(flags["--expect"])) != nil {
		return fail("--expect requires a full index revision hash")
	}
	valid := true
	switch cmd {
	case "log", "list":
		choices := 0
		if len(pos) > 0 {
			choices++
		}
		for _, flag := range []string{"--branch", "--all", "--retained"} {
			if p.has(flag) {
				choices++
			}
		}
		if choices > 1 {
			return fail("select one ref, --branch, --all, or --retained")
		}
		if p.has("--server") && (p.has("--all") || p.has("--retained")) {
			return fail("--server requires one selected history")
		}
	case "repo":
		valid = sub == "create"
	case "add":
		for _, value := range pos {
			if value != "claude" && value != "codex" && value != "." {
				return fail("provider must be claude or codex (or . for both)")
			}
		}
	case "commit":
		if p.has("--resume") && (p.has("-m") || p.has("--expect")) {
			return fail("--resume replays the saved operation; it cannot change its message or revision")
		}
	case "restore":
		if !p.has("--staged") {
			return fail("restore requires --staged; provider conversations are never deleted")
		}
		for _, key := range pos {
			if key == "." && len(pos) != 1 {
				return fail("use . alone to unstage every source")
			}
			if key != "." && domain.ValidateContentHash(domain.ContentHash(key)) != nil {
				return fail("source keys must be full sha256 hashes from cxt status")
			}
		}
	case "fork":
		valid = flags["--as"] != ""
	case "load":
		if p.has("--work-state") && p.has("--mode") {
			return fail("personal handoff cannot be combined with legacy replay --mode")
		}
		if p.has("--context-budget") {
			if _, err := domain.ParseHistoryBudget(flags["--context-budget"]); err != nil {
				return fail(err.Error())
			}
			if !p.has("--output") {
				return fail("history artifact preparation requires --output; use cxt --pull <provider> for verified native delivery")
			}
		}
		if p.has("--output") && p.has("--mode") {
			return fail("artifact input policy is separate from legacy replay --mode")
		}
		if p.has("--model") && !p.has("--output") {
			return fail("--model requires --output")
		}
	case "pull", "fetch":
		if cmd == "pull" && p.has("--force") {
			return fail("--force is no longer supported; preview an exact pointer with cxt repair --preview, then apply the reviewed plan")
		}
		if len(p.positionals) == 2 && domain.ValidateBranchName(p.positionals[1]) != nil {
			return fmt.Errorf("expected a branch name; refspec mappings are not supported")
		}
	case "push":
		if len(p.positionals) == 2 && domain.ValidateBranchName(p.positionals[1]) != nil {
			return fmt.Errorf("expected a branch name; refspec mappings are not supported")
		}
		valid = flags["--force"] == "" || flags["--append"] == ""
	case "login":
		valid = len(pos) == 0 || flags["-t"] == ""
	case "mcp":
		if flags["--local"] == "" {
			return fail("the product connector is https://cxthub.com/mcp; the stdio helper requires --local")
		}
	case "repair":
		switch {
		case p.has("--from-server"):
			valid = only("--from-server", "--remote")
		case p.has("--preview"):
			valid = only("--preview", "--ref", "--snapshot", "--reason", "--output") && p.has("--reason") && p.has("--output") && p.has("--ref") != p.has("--snapshot")
		case p.has("--apply"):
			valid = only("--apply", "--expect") && domain.ValidateContentHash(domain.ContentHash(flags["--expect"])) == nil
		default:
			valid = false
		}
	case "hook":
		valid = flags["--provider"] != "" && flags["--event"] != ""
	case "sync":
		valid = sub == "status"
	case "hooks":
		valid = sub == "install" || sub == "uninstall"
	case "config":
		valid = false
		for _, key := range []string{"checkout.mode", "load.mode", "boundary.enforce", "capture.debounce", "secrets.scrub", "secrets.redact", "secrets.minlen"} {
			if sub == key {
				valid = true
			}
		}
		if len(pos) == 2 {
			p.effect = commandLocalWrite
		}
	case "tag":
		if len(pos) != 0 {
			p.effect = commandContextWrite
		}
	case "remote":
		switch sub {
		case "":
			valid = len(pos) == 0
		case "add":
			valid = len(pos) == 3 && only()
			p.effect = commandLocalWrite
		case "remove", "rm":
			valid = len(pos) == 2 && only()
			p.effect = commandLocalWrite
		default:
			valid = false
		}
	case "branch":
		switch sub {
		case "":
			valid = len(pos) == 0 && only()
		case "list":
			valid = len(pos) == 1 && only()
		case "operations":
			valid = len(pos) == 1 && only("--json")
		case "replay":
			valid = len(pos) == 1 && only()
			p.effect = commandContextWrite
		case "recover":
			valid = len(pos) == 2 && only("--confirm-orphan") && flags["--confirm-orphan"] != ""
			p.effect = commandContextWrite
		case "archive":
			valid = len(pos) == 2 && only()
			p.effect = commandContextWrite
		case "restore":
			valid = len(pos) == 2 && only("--provider", "--mode")
			p.effect = commandContextWrite
		default:
			valid = false
		}
	case "capture":
		switch sub {
		case "list":
			valid = len(pos) == 1 && only("--all", "--json")
		case "show":
			valid = len(pos) == 2 && only("--json")
		case "retry":
			valid = len(pos) == 2 && only("--expect") && flags["--expect"] != ""
			p.effect = commandSync
		case "resolve":
			valid = len(pos) == 2 && only("--expect", "--json") && flags["--expect"] != ""
			p.effect = commandLocalWrite
		case "acknowledge":
			valid = len(pos) == 2 && only("--expect", "--reason", "--json") && flags["--expect"] != "" && flags["--reason"] != ""
			p.effect = commandLocalWrite
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
			p.effect = commandLocalWrite
		case "restore":
			p.effect = commandLocalWrite
			if len(pos) == 2 {
				n, err := strconv.Atoi(pos[1])
				valid = err == nil && n >= 0
			}
		default:
			valid = false
		}
	case "stash":
		if p.has("--staged") {
			switch sub {
			case "", "push":
				valid = only("--staged", "--expect", "--json")
			case "pop":
				valid = only("--staged", "--expect", "--id", "--json") && p.has("--id")
			case "list":
				valid = only("--staged", "--json")
				p.effect = commandRead
			default:
				valid = false
			}
			break
		}
		switch sub {
		case "", "push":
			valid = only("-m", "--provider")
		case "pop":
			valid = only()
		case "list":
			valid = only()
			p.effect = commandRead
		default:
			valid = false
		}
	}
	if !valid {
		return fail("unsupported argument or flag combination")
	}
	return nil
}

// ReadOnlyInvocation is used by composition to avoid constructing capture,
// materialization, distillation, sync mutation, and journal-replay services.
func ReadOnlyInvocation(args []string) bool {
	if len(args) < 2 {
		return true
	}
	p, help, err := parseCommand(args[1], args[2:])
	return err == nil && (help || p.effect == commandRead)
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
	sort.Strings(names)
	return names
}

// Legacy internal hooks still use these raw-argv helpers. Public validation
// uses only the selected command's option definitions above.
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
