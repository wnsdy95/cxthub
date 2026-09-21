package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// GitCreation is immutable local evidence, not a GitHub attestation. Command
// contains only branch-creation arguments; global Git config and worktree paths
// are deliberately excluded. Unknown/legacy commands must never be invented.
type GitCreation struct {
	Evidence       string   `json:"evidence"`
	Command        []string `json:"command,omitempty"`
	StartRef       string   `json:"start_ref,omitempty"`
	StartCommit    string   `json:"start_commit,omitempty"`
	OriginBranch   string   `json:"origin_branch,omitempty"`
	OriginBranchID string   `json:"origin_branch_id,omitempty"`
}

func ValidateGitCreation(e HistoryEvent) error {
	c := e.Creation
	if c == nil {
		return nil
	}
	if e.Kind != "birth" && e.Kind != "orphan" && e.Kind != "attach" {
		return fmt.Errorf("creation evidence requires branch creation")
	}
	if c.Evidence != "process-argv" && c.Evidence != "unavailable" {
		return fmt.Errorf("invalid creation evidence source")
	}
	if c.Evidence == "unavailable" && (len(c.Command) > 0 || c.StartRef != "" || c.OriginBranch != "" || c.OriginBranchID != "") {
		return fmt.Errorf("unknown creation must not claim command or origin")
	}
	if c.Evidence == "process-argv" {
		if len(c.Command) < 3 || len(c.Command) > 12 || c.Command[0] != "git" || c.StartRef == "" {
			return fmt.Errorf("invalid creation command")
		}
		switch c.Command[1] {
		case "checkout", "switch", "branch", "worktree":
			if err := validateCreationCommand(e); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid creation operation")
		}
	}
	for _, value := range append(append([]string{}, c.Command...), c.StartRef, c.OriginBranch, c.OriginBranchID) {
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "://") {
			return fmt.Errorf("invalid creation argument")
		}
	}
	if (c.OriginBranch == "") != (c.OriginBranchID == "") {
		return fmt.Errorf("incomplete origin identity")
	}
	if c.OriginBranch != "" {
		if err := ValidateBranchName(c.OriginBranch); err != nil {
			return err
		}
	}
	if c.Evidence == "process-argv" && e.GitAfter != "" && c.StartCommit == "" {
		return fmt.Errorf("creation commit is missing")
	}
	if c.StartCommit != "" {
		if len(c.StartCommit) != 40 && len(c.StartCommit) != 64 {
			return fmt.Errorf("invalid creation commit")
		}
		if _, err := hex.DecodeString(c.StartCommit); err != nil {
			return fmt.Errorf("invalid creation commit")
		}
		expected := e.GitAfter
		if e.Kind == "orphan" {
			expected = e.GitBefore
		}
		if c.StartCommit != expected {
			return fmt.Errorf("creation start commit differs from Git transaction")
		}
	}
	return nil
}

// A delayed upload may refer to an old name; resolve against retained events,
// never today's mutable name binding. Legacy identities remain explicit.
func ValidateCreationOrigin(history []HistoryEvent, e HistoryEvent) error {
	c := e.Creation
	if c == nil || c.OriginBranchID == "" {
		return nil
	}
	if e.Kind != "attach" && c.OriginBranchID == e.BranchID {
		return fmt.Errorf("branch cannot be its own creation origin")
	}
	if c.OriginBranchID == LegacyContextBranchID(e.RepoID, c.OriginBranch) {
		return nil
	}
	for _, prior := range history {
		if prior.RepoID == e.RepoID && prior.BranchID == c.OriginBranchID && (prior.Branch == c.OriginBranch || (prior.Kind == "rename" && prior.PreviousBranch == c.OriginBranch)) {
			return nil
		}
	}
	return fmt.Errorf("creation origin identity has no retained branch evidence")
}

// Validate the recorded command independently from the capture adapter. A
// mismatched named start or target cannot masquerade as a valid checkpoint.
func validateCreationCommand(e HistoryEvent) error {
	c := e.Creation
	args := c.Command[2:]
	op := c.Command[1]
	target, start := "", "HEAD"
	orphan := false
	track := false
	if op == "worktree" {
		if len(args) == 0 || args[0] != "add" {
			return fmt.Errorf("invalid worktree creation")
		}
		args = args[1:]
	}
	positional := []string{}
	for i := 0; i < len(args); i++ {
		v := args[i]
		switch v {
		case "-b", "-B", "-c", "-C", "--create", "--force-create", "--orphan":
			if op == "branch" || target != "" || i+1 >= len(args) {
				return fmt.Errorf("invalid creation arguments")
			}
			i++
			target = args[i]
			orphan = v == "--orphan"
		case "--track", "-t", "--track=direct":
			track = true
		case "--track=inherit", "--no-track", "--no-guess", "-q", "--quiet":
		default:
			if strings.HasPrefix(v, "-") {
				return fmt.Errorf("unsupported creation argument")
			}
			positional = append(positional, v)
		}
	}
	if op == "worktree" {
		if len(positional) == 0 || positional[0] != "<worktree>" {
			return fmt.Errorf("worktree path must be redacted")
		}
		positional = positional[1:]
	}
	if op == "branch" {
		if len(positional) == 0 {
			return fmt.Errorf("missing created branch")
		}
		target = positional[0]
		positional = positional[1:]
	}
	want := e.Branch
	if e.LocalBranch != "" {
		want = e.LocalBranch
	}
	if target == "" && track && len(positional) == 1 && strings.HasSuffix(positional[0], "/"+want) {
		target = want
	}
	if len(positional) > 1 {
		return fmt.Errorf("ambiguous creation start")
	}
	if len(positional) == 1 {
		start = positional[0]
	}
	if target != want || start != c.StartRef || orphan != (e.Kind == "orphan") {
		return fmt.Errorf("command disagrees with recorded branch or start")
	}
	return nil
}
