package cli

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// parseCreationCommand retains only an allowlisted creation command. In
// particular -c credentials, paths, environment and commit messages never leave
// this process. Unknown option layouts are evidence-unavailable, not guessed.
func parseCreationCommand(argv []string, target string) (*domain.GitCreation, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return nil, false
	}
	i := 1
	for i < len(argv) {
		a := argv[i]
		if a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree" {
			i += 2
			continue
		}
		if strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") || strings.HasPrefix(a, "--config-env=") || a == "--no-pager" {
			i++
			continue
		}
		break
	}
	if i >= len(argv) {
		return nil, false
	}
	op := argv[i]
	args := argv[i+1:]
	switch op {
	case "switch", "checkout", "branch", "worktree":
	default:
		return nil, false
	}
	if op == "worktree" {
		if len(args) == 0 || args[0] != "add" {
			return nil, false
		}
		args = args[1:]
	}
	command := []string{"git", op}
	if op == "worktree" {
		command = append(command, "add")
	}
	name, start := "", "HEAD"
	positional := []string{}
	for n := 0; n < len(args); n++ {
		a := args[n]
		switch a {
		case "-b", "-B", "-c", "-C", "--create", "--force-create", "--orphan":
			if op == "branch" || n+1 >= len(args) || name != "" {
				return nil, false
			}
			n++
			name = args[n]
			command = append(command, a, name)
		case "--track", "--track=direct", "--track=inherit", "-t", "--no-track", "--no-guess", "-q", "--quiet":
			command = append(command, a)
		case "--":
			positional = append(positional, args[n+1:]...)
			n = len(args)
		default:
			if strings.HasPrefix(a, "-") {
				return nil, false
			}
			positional = append(positional, a)
		}
	}
	if op == "worktree" {
		if len(positional) < 1 {
			return nil, false
		}
		command = append(command, "<worktree>")
		positional = positional[1:]
	}
	if op == "branch" {
		if len(positional) < 1 {
			return nil, false
		}
		name = positional[0]
		positional = positional[1:]
		command = append(command, name)
	}
	if name == "" { // --track origin/x creates x; ordinary switches never create evidence.
		track := false
		for _, v := range command {
			if v == "--track" || v == "-t" || v == "--track=direct" {
				track = true
			}
		}
		if !track || len(positional) != 1 || !strings.HasSuffix(positional[0], "/"+target) {
			return nil, false
		}
		name = target
	}
	if name != target || domain.ValidateBranchName(name) != nil || len(positional) > 1 {
		return nil, false
	}
	if len(positional) == 1 {
		start = positional[0]
		command = append(command, start)
	}
	for _, v := range append(append([]string{}, command...), start) {
		if len(v) > 512 || strings.ContainsAny(v, "\x00\r\n\t ") && v != "<worktree>" || strings.Contains(v, "://") {
			return nil, false
		}
	}
	return &domain.GitCreation{Evidence: "process-argv", Command: command, StartRef: start}, true
}

func captureGitCreation(ctx context.Context, cwd, pid, target, before, after string, orphan bool) *domain.GitCreation {
	unknown := &domain.GitCreation{Evidence: "unavailable", StartCommit: after}
	if orphan {
		unknown.StartCommit = before
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 1 {
		return unknown
	}
	argv, err := readProcessArgv(n)
	if err != nil {
		return unknown
	}
	c, ok := parseCreationCommand(argv, target)
	if !ok {
		return unknown
	}
	c.StartCommit = gitOut(cwd, "rev-parse", "--verify", c.StartRef+"^{commit}")
	if !orphan && c.StartCommit != after {
		return unknown
	}
	if orphan && c.StartRef == "HEAD" && c.StartCommit != before {
		return unknown
	}
	ref := gitOut(cwd, "rev-parse", "--symbolic-full-name", c.StartRef)
	if strings.HasPrefix(ref, "refs/heads/") {
		c.OriginBranch = strings.TrimPrefix(ref, "refs/heads/")
	} else if strings.HasPrefix(ref, "refs/remotes/") {
		longest := ""
		for _, remote := range strings.Split(gitOut(cwd, "remote"), "\n") {
			prefix := "refs/remotes/" + remote + "/"
			if remote != "" && strings.HasPrefix(ref, prefix) && len(prefix) > len(longest) {
				longest = prefix
			}
		}
		if longest != "" {
			c.OriginBranch = strings.TrimPrefix(ref, longest)
		}
	}
	return c
}
