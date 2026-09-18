package domain

// CodeSelection is independent of context position. A snapshot may have been
// published at several code commits; callers must supply the intended SHA.
type CodeSelection struct {
	CodeCommit   string   `json:"code_commit"`
	SourceCommit string   `json:"source_commit"`
	SourceParent string   `json:"source_parent,omitempty"`
	Paths        []string `json:"paths"`
}

func (s CodeSelection) Validate() error {
	if ValidateGitOID(s.CodeCommit) != nil || ValidateGitOID(s.SourceCommit) != nil || (s.SourceParent != "" && ValidateGitOID(s.SourceParent) != nil) || len(s.Paths) == 0 || len(s.Paths) > 100 {
		return ErrValidation
	}
	seen := map[string]bool{}
	for _, p := range s.Paths {
		if len(p) > 4096 || ValidateGitPath(p) != nil || seen[p] {
			return ErrValidation
		}
		seen[p] = true
	}
	return nil
}

type CodePathState struct {
	Path     string   `json:"path"`
	State    string   `json:"state"` // applied, before, changed, equivalent, not_in_history, unknown
	Reason   string   `json:"reason,omitempty"`
	Before   GitEntry `json:"before"`
	After    GitEntry `json:"after"`
	Selected GitEntry `json:"selected"`
}
type CodeApplicability struct {
	Selection CodeSelection      `json:"selection"`
	Revision  RepositoryRevision `json:"revision"`
	StateHash ContentHash        `json:"state_hash"`
	Relation  string             `json:"relation"` // ancestor, not_ancestor, unknown
	Paths     []CodePathState    `json:"paths"`
	Reason    string             `json:"reason,omitempty"`
}

// AssessCodePath reports observable file state, not semantic correctness of
// arbitrary prose. "before" is not proof that a named revert occurred.
func AssessCodePath(change GitPathChange, selected GitEntry, relation string) CodePathState {
	out := CodePathState{Path: change.Path, Before: change.Before, After: change.After, Selected: selected, State: "unknown"}
	switch relation {
	case "ancestor":
		switch selected {
		case change.After:
			out.State = "applied"
		case change.Before:
			out.State = "before"
		default:
			out.State = "changed"
		}
	case "not_ancestor":
		out.State = "not_in_history"
		if selected == change.After {
			out.State = "equivalent"
		}
	default:
		out.Reason = "incomplete_ancestry"
	}
	return out
}
