package cli

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Only an explicit tracking creation joins a teammate's context. In particular,
// Git's automatic upstream for `switch -c feature origin/main` does not turn a
// new task into main, and a later push -u cannot change the original decision.
func creationTracking(e domain.HistoryEvent) string {
	if e.Kind == "orphan" {
		return "birth"
	}
	c := e.Creation
	if c == nil || c.Evidence != "process-argv" {
		return "unavailable"
	}
	parsed, ok := parseCreationCommand(c.Command, e.Branch)
	if !ok || parsed.StartRef != c.StartRef {
		return "unavailable"
	}
	mode := "birth"
	for _, arg := range parsed.Command {
		switch arg {
		case "--track", "--track=direct", "-t":
			mode = "direct"
		case "--track=inherit":
			mode = "inherit"
		case "--no-track":
			mode = "birth"
		}
	}
	return mode
}

func legacyBranchBinding(e domain.HistoryEvent) *branchjournal.BindingIntent {
	mode := creationTracking(e)
	if mode == "birth" {
		return &branchjournal.BindingIntent{Kind: "birth"}
	}
	if mode == "direct" {
		c := e.Creation
		// OriginBranch was frozen from the symbolic source ref at creation. A
		// local branch called origin/x retains that entire name, unlike the
		// remote-tracking ref origin/x whose recorded branch is x.
		if c.OriginBranch != "" {
			if c.StartRef == "HEAD" || c.StartRef == c.OriginBranch || c.StartRef == "refs/heads/"+c.OriginBranch {
				return &branchjournal.BindingIntent{Kind: "birth"}
			}
			if strings.HasSuffix(c.StartRef, "/"+c.OriginBranch) {
				return &branchjournal.BindingIntent{Kind: "attach", RemoteBranch: c.OriginBranch}
			}
		}
	}
	return &branchjournal.BindingIntent{Kind: "unavailable"}
}

// Freeze local configuration while the Git creation vote is being prepared.
// No network runs here. If evidence is insufficient, the durable source remains
// available but replay cannot silently infer a different logical branch.
func freezeBranchBinding(cwd string, e domain.HistoryEvent, gitPIDs ...string) *branchjournal.BindingIntent {
	mode := creationTracking(e)
	// The first commit creates the unborn HEAD ref without a branch command.
	// Prove that command while it is still alive; a missing HEAD alone could
	// also precede an explicit tracking checkout and is not sufficient evidence.
	if mode == "unavailable" && e.GitBefore == "" && e.Branch == gitOut(cwd, "symbolic-ref", "--short", "HEAD") && len(gitPIDs) > 0 {
		pid, err := strconv.Atoi(gitPIDs[0])
		if err == nil && pid > 1 {
			argv, err := readProcessArgv(pid)
			if err == nil && initialCommitCommand(argv) {
				mode = "birth"
			}
		}
	}
	if mode == "birth" || mode == "unavailable" {
		return &branchjournal.BindingIntent{Kind: mode}
	}
	c := e.Creation
	ref := gitOut(cwd, "rev-parse", "--symbolic-full-name", c.StartRef)
	if gitOut(cwd, "rev-parse", "--verify", c.StartRef+"^{commit}") != c.StartCommit {
		return &branchjournal.BindingIntent{Kind: "unavailable"}
	}
	if mode == "inherit" && strings.HasPrefix(ref, "refs/heads/") {
		// for-each-ref reads the source branch, never the not-yet-created target.
		parts := strings.Split(gitOut(cwd, "for-each-ref", "--format=%(refname)%00%(upstream:remotename)%00%(upstream:remoteref)", ref), "\x00")
		if len(parts) != 3 || parts[0] != ref {
			return &branchjournal.BindingIntent{Kind: "unavailable"}
		}
		remote, merge := parts[1], parts[2]
		if remote == "." {
			return &branchjournal.BindingIntent{Kind: "birth"}
		}
		if remote != "" && strings.HasPrefix(merge, "refs/heads/") {
			branch := strings.TrimPrefix(merge, "refs/heads/")
			if domain.ValidateBranchName(branch) == nil {
				return &branchjournal.BindingIntent{Kind: "attach", RemoteBranch: branch}
			}
		}
		return &branchjournal.BindingIntent{Kind: "unavailable"}
	}
	if mode == "direct" {
		if strings.HasPrefix(ref, "refs/heads/") {
			return &branchjournal.BindingIntent{Kind: "birth"}
		}
		if strings.HasPrefix(ref, "refs/remotes/") {
			return legacyBranchBinding(e)
		}
	}
	return &branchjournal.BindingIntent{Kind: "unavailable"}
}

func initialCommitCommand(argv []string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return false
	}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") || strings.HasPrefix(a, "--config-env=") || a == "--no-pager" {
			continue
		}
		return a == "commit"
	}
	return false
}
