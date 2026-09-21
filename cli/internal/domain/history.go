package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// HistoryEvent records an observed operation independently of conversation
// content. ID is generated once before Git commits, and reused on every retry.
// Source is conversation ancestry; MemorySource is provenance only (orphan).
type HistoryEvent struct {
	Creation *GitCreation `json:"creation,omitempty"`
	// PRCompleted marks a separate server receipt issued only after successful promotion.
	PRCompleted      bool              `json:"pr_completed,omitempty"`
	RecoveryEvidence string            `json:"recovery_evidence,omitempty"`
	PR               *PullRequestMerge `json:"pr,omitempty"`
	SourceBranchID   string            `json:"source_branch_id,omitempty"`
	ID               string            `json:"id"`
	RepoID           string            `json:"repo_id"`
	BranchID         string            `json:"branch_id"`
	Branch           string            `json:"branch"`
	Kind             string            `json:"kind"`
	LocalBranch      string            `json:"local_branch,omitempty"`
	PreviousBranch   string            `json:"previous_branch,omitempty"`
	BindingParent    string            `json:"binding_parent,omitempty"`
	NameParent       string            `json:"name_parent,omitempty"`
	Source           ContentHash       `json:"source,omitempty"`
	Target           ContentHash       `json:"target,omitempty"`
	SharedTarget     ContentHash       `json:"shared_target,omitempty"`
	MemorySource     ContentHash       `json:"memory_source,omitempty"`
	MemoryHash       ContentHash       `json:"memory_hash,omitempty"`
	MemoryPinned     bool              `json:"memory_pinned,omitempty"`
	GitBefore        string            `json:"git_before,omitempty"`
	GitAfter         string            `json:"git_after,omitempty"`
	WorktreeID       string            `json:"worktree_id,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

func ValidateHistoryEvent(e HistoryEvent) error {
	if err := ValidateGitCreation(e); err != nil {
		return err
	}
	if len(e.ID) != 32 || e.ID != strings.ToLower(e.ID) {
		return fmt.Errorf("invalid history operation ID")
	}
	if _, err := hex.DecodeString(e.ID); err != nil {
		return fmt.Errorf("invalid history operation ID")
	}
	if e.BranchID == "" || len(e.BranchID) > 128 {
		return fmt.Errorf("invalid context branch identity")
	}
	if err := ValidateContentHash(ContentHash(e.RepoID)); err != nil {
		return err
	}
	if e.Branch != "" || e.Kind != "position" {
		if err := ValidateBranchName(e.Branch); err != nil {
			return err
		}
	}
	switch e.Kind {
	case "birth", "attach", "orphan", "position", "publish", "advance", "rename", "archive", "pr-merge":
	default:
		return fmt.Errorf("invalid history event kind")
	}
	if e.Kind == "publish" && (e.Target == "" || e.Source != e.Target || e.GitAfter == "" || e.GitAfter != strings.ToLower(e.GitAfter) || strings.Trim(e.GitAfter, "0") == "") {
		return fmt.Errorf("publication requires an exact Git revision and source target")
	}
	if e.RecoveryEvidence != "" && (e.Kind != "orphan" || e.RecoveryEvidence != "user-confirmed-unborn-head") {
		return fmt.Errorf("invalid recovery evidence")
	}
	if e.Kind == "pr-merge" {
		if e.PR == nil || e.SourceBranchID == "" || len(e.SourceBranchID) > 128 || e.Source == "" || e.Target == "" || (!e.PRCompleted && (e.Target != e.Source || e.Branch != e.PR.BaseBranch)) {
			return fmt.Errorf("invalid PR context binding")
		}
		if err := e.PR.Validate(); err != nil {
			return err
		}
	} else if e.PR != nil || e.SourceBranchID != "" || e.PRCompleted {
		return fmt.Errorf("unexpected PR metadata")
	}
	if e.LocalBranch != "" {
		if err := ValidateBranchName(e.LocalBranch); err != nil {
			return err
		}
	}
	if e.Kind == "rename" {
		if err := ValidateBranchName(e.PreviousBranch); err != nil {
			return err
		}
		if e.PreviousBranch == e.Branch {
			return fmt.Errorf("rename requires distinct names")
		}
	} else if e.PreviousBranch != "" || e.NameParent != "" {
		return fmt.Errorf("unexpected rename metadata")
	}
	for _, parent := range []string{e.BindingParent, e.NameParent} {
		if parent == "" {
			continue
		}
		if len(parent) != 32 || parent != strings.ToLower(parent) || parent == e.ID {
			return fmt.Errorf("invalid branch dependency")
		}
		if _, err := hex.DecodeString(parent); err != nil {
			return fmt.Errorf("invalid branch dependency")
		}
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("missing history event time")
	}
	for _, h := range []ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource, e.MemoryHash} {
		if err := ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	for _, oid := range []string{e.GitBefore, e.GitAfter} {
		if oid == "" {
			continue
		}
		if len(oid) != 40 && len(oid) != 64 {
			return fmt.Errorf("invalid Git object ID")
		}
		if _, err := hex.DecodeString(oid); err != nil {
			return fmt.Errorf("invalid Git object ID")
		}
	}
	if e.WorktreeID != "" {
		if len(e.WorktreeID) != 32 || e.WorktreeID != strings.ToLower(e.WorktreeID) {
			return fmt.Errorf("invalid worktree identity")
		}
		if _, err := hex.DecodeString(e.WorktreeID); err != nil {
			return fmt.Errorf("invalid worktree identity")
		}
	}
	if e.Kind == "advance" && (e.Source == "" || e.Target == "") {
		return fmt.Errorf("continuation requires both previous and current context")
	}
	if e.Kind == "orphan" && (e.Source != "" || e.Target != "") {
		return fmt.Errorf("orphan creation cannot inherit conversation ancestry")
	}
	return nil
}
