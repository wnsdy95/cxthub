package domain

import "fmt"

// ValidateContextRefWrite runs inside the repository graph transaction. Matching
// content hashes cannot authorize a different logical branch after name reuse.
func ValidateContextRefWrite(protocol int, events []HistoryEvent, current *Ref, next Ref) error {
	if protocol == 0 {
		return nil
	}
	if protocol != 1 {
		return fmt.Errorf("%w: unsupported context protocol", ErrConflict)
	}
	if next.Kind != RefBranch {
		return nil
	}
	if next.BranchID == "" {
		return fmt.Errorf("%w: branch identity required; upgrade the CXTHub client for this repository", ErrConflict)
	}
	p, err := ProjectContextBranches(events)
	if err != nil {
		return err
	}
	if b, ok := p.Active[next.Name]; ok {
		if b.ID != next.BranchID {
			return fmt.Errorf("%w: branch name now belongs to another context identity", ErrRefConflict)
		}
	} else {
		if p.Released[next.Name] != "" || next.BranchID != LegacyContextBranchID(string(next.RepoID), next.Name) || current == nil {
			return fmt.Errorf("%w: a durable branch birth is required", ErrRefConflict)
		}
	}
	if current != nil && current.BranchID != next.BranchID {
		return fmt.Errorf("%w: current branch identity differs from requested identity", ErrRefConflict)
	}
	return nil
}

// ContextProtocolRefs migrates confirmed live pointers without inventing old
// birth events. Reused names with no identity proof cannot be upgraded by guess.
func ContextProtocolRefs(repo ContentHash, refs []Ref, events []HistoryEvent) ([]Ref, error) {
	p, err := ProjectContextBranches(events)
	if err != nil {
		return nil, err
	}
	projected, err := ProjectBranchLifecycleRefs(refs)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(projected))
	for _, ref := range projected {
		if ref.Kind != RefBranch {
			continue
		}
		identity := LegacyContextBranchID(string(repo), ref.Name)
		if b, ok := p.Active[ref.Name]; ok {
			identity = b.ID
			if p.Released[ref.Name] != "" && ref.BranchID != identity {
				return nil, fmt.Errorf("%w: reused branch %q needs an identity-aware sync before upgrade", ErrConflict, ref.Name)
			}
		} else if p.Released[ref.Name] != "" {
			return nil, fmt.Errorf("%w: branch %q has an unfinished archive/rename projection", ErrConflict, ref.Name)
		}
		if ref.BranchID != "" && ref.BranchID != identity {
			return nil, fmt.Errorf("%w: branch %q identity disagrees with history", ErrConflict, ref.Name)
		}
		ref.BranchID = identity
		out = append(out, ref)
	}
	return out, nil
}
