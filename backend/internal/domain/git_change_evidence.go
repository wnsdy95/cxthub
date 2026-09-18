package domain

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// GitEntry identifies bytes AND mode, including executable files, symlinks and
// submodule gitlinks. A zero entry represents absence. Renames are represented
// as the delete/add changes of exact paths, without similarity heuristics.
type GitEntry struct {
	OID  string `json:"oid,omitempty"`
	Mode string `json:"mode,omitempty"`
}

type GitPathChange struct {
	Path   string   `json:"path"`
	Before GitEntry `json:"before"`
	After  GitEntry `json:"after"`
}

// GitCommitDelta is read from the repository's Git provider. Parent is the
// explicitly selected comparison parent; for a merge it must never be guessed.
// Complete is false for truncated/paginated evidence that has not been fully
// retrieved. Adapters must not accept these values as client-verified claims.
type GitCommitDelta struct {
	Commit   string          `json:"commit"`
	Parents  []string        `json:"parents"`
	Parent   string          `json:"parent,omitempty"`
	Changes  []GitPathChange `json:"changes"`
	Complete bool            `json:"complete"`
}

// GitReversalEvidence describes only demonstrable inverse file changes. It is
// not a global PR status and must not erase the historical promotion receipt.
// The application additionally verifies repository ownership and that Target
// is an ancestor of the selected comparison parent of Commit.
type GitReversalEvidence struct {
	Commit          string   `json:"commit"`
	Target          string   `json:"target"`
	Parent          string   `json:"parent"`
	TargetParent    string   `json:"target_parent,omitempty"`
	Coverage        string   `json:"coverage"` // full, partial, or unverified
	Paths           []string `json:"paths"`
	UnverifiedPaths []string `json:"unverified_paths"`
	OtherPaths      []string `json:"other_paths"`
	Reason          string   `json:"reason,omitempty"`
}

func ValidateGitOID(oid string) error {
	if (len(oid) != 40 && len(oid) != 64) || oid != strings.ToLower(oid) || strings.Trim(oid, "0") == "" {
		return fmt.Errorf("%w: full nonzero Git object ID required", ErrValidation)
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return fmt.Errorf("%w: invalid Git object ID", ErrValidation)
	}
	return nil
}

func validateGitEntry(entry GitEntry) error {
	if entry.OID == "" && entry.Mode == "" {
		return nil
	}
	if err := ValidateGitOID(entry.OID); err != nil {
		return err
	}
	switch entry.Mode {
	case "100644", "100755", "120000", "160000":
		return nil
	default:
		return fmt.Errorf("%w: invalid Git entry mode", ErrValidation)
	}
}

func (delta GitCommitDelta) Validate() error {
	if err := ValidateGitOID(delta.Commit); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range delta.Parents {
		if err := ValidateGitOID(p); err != nil {
			return err
		}
		if p == delta.Commit || seen[p] {
			return fmt.Errorf("%w: duplicate/self Git parent", ErrValidation)
		}
		seen[p] = true
	}
	if len(delta.Parents) > 0 && !seen[delta.Parent] {
		return fmt.Errorf("%w: explicit comparison parent required", ErrValidation)
	}
	if len(delta.Parents) == 0 && delta.Parent != "" {
		return fmt.Errorf("%w: root commit has no comparison parent", ErrValidation)
	}
	paths := map[string]bool{}
	for _, c := range delta.Changes {
		if err := ValidateGitPath(c.Path); err != nil {
			return err
		}
		if paths[c.Path] {
			return fmt.Errorf("%w: duplicate Git path", ErrValidation)
		}
		paths[c.Path] = true
		if err := validateGitEntry(c.Before); err != nil {
			return err
		}
		if err := validateGitEntry(c.After); err != nil {
			return err
		}
		if c.Before == c.After {
			return fmt.Errorf("%w: unchanged Git entry in delta", ErrValidation)
		}
	}
	return nil
}

// AssessGitReversal does not examine commit titles/messages. Only byte/mode
// inversions are automatic. A line-level partial revert inside a changed file
// stays unverified until a stronger, range-aware proof is available.
func AssessGitReversal(target, candidate GitCommitDelta) (GitReversalEvidence, error) {
	out := GitReversalEvidence{Commit: candidate.Commit, Target: target.Commit, Parent: candidate.Parent, TargetParent: target.Parent, Coverage: "unverified", Paths: []string{}, UnverifiedPaths: []string{}, OtherPaths: []string{}}
	if err := target.Validate(); err != nil {
		return out, err
	}
	if err := candidate.Validate(); err != nil {
		return out, err
	}
	if target.Commit == candidate.Commit {
		return out, fmt.Errorf("%w: a commit cannot reverse itself", ErrValidation)
	}
	if !target.Complete || !candidate.Complete {
		out.Reason = "incomplete_git_evidence"
		return out, nil
	}
	if len(target.Changes) == 0 {
		out.Reason = "target_has_no_changes"
		return out, nil
	}
	if candidate.Parent == "" {
		out.Reason = "candidate_has_no_parent"
		return out, nil
	}
	byPath := map[string]GitPathChange{}
	for _, change := range candidate.Changes {
		byPath[change.Path] = change
	}
	targeted := map[string]bool{}
	for _, before := range target.Changes {
		targeted[before.Path] = true
		after, ok := byPath[before.Path]
		if ok && before.Before == after.After && before.After == after.Before {
			out.Paths = append(out.Paths, before.Path)
		} else {
			out.UnverifiedPaths = append(out.UnverifiedPaths, before.Path)
		}
	}
	for _, change := range candidate.Changes {
		if !targeted[change.Path] {
			out.OtherPaths = append(out.OtherPaths, change.Path)
		}
	}
	sort.Strings(out.Paths)
	sort.Strings(out.UnverifiedPaths)
	sort.Strings(out.OtherPaths)
	switch {
	case len(out.Paths) == len(target.Changes):
		out.Coverage = "full"
	case len(out.Paths) > 0:
		out.Coverage = "partial"
		out.Reason = "remaining_paths_need_review"
	default:
		out.Reason = "no_exact_inverse"
	}
	return out, nil
}

func ValidateGitPath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return fmt.Errorf("%w: invalid Git path", ErrValidation)
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: invalid Git path", ErrValidation)
		}
	}
	return nil
}
func (e GitReversalEvidence) Validate() error {
	for _, oid := range []string{e.Commit, e.Target} {
		if err := ValidateGitOID(oid); err != nil {
			return err
		}
	}
	for _, oid := range []string{e.Parent, e.TargetParent} {
		if oid != "" {
			if err := ValidateGitOID(oid); err != nil {
				return err
			}
		}
	}
	if e.Commit == e.Target {
		return ErrIntegrity
	}
	switch e.Coverage {
	case "full":
		if len(e.Paths) == 0 || len(e.UnverifiedPaths) != 0 || e.Parent == "" {
			return ErrIntegrity
		}
	case "partial":
		if len(e.Paths) == 0 || len(e.UnverifiedPaths) == 0 || e.Parent == "" {
			return ErrIntegrity
		}
	case "unverified":
		if len(e.Paths) != 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	seen := map[string]bool{}
	for _, paths := range [][]string{e.Paths, e.UnverifiedPaths, e.OtherPaths} {
		for _, p := range paths {
			if err := ValidateGitPath(p); err != nil {
				return err
			}
			if seen[p] {
				return ErrIntegrity
			}
			seen[p] = true
		}
	}
	return nil
}
