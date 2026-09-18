package domain

import (
	"encoding/json"
	"strings"
)

// MergeDigests inherits memory (prior) into new distillation (fresh) — deterministic.
//
// Memory follows the same logic as raw ancestry: if the snapshot ancestry continues (natural inheritance·append graft irrelevant), memory also continues. Continuous commits in the same session result in deterministic distillation recreating the same items, so dedup absorbs, and new sessions/appends preserve prior items (ancestor precedence).
func MergeDigests(prior, fresh MemoryDigest) MemoryDigest {
	if len(memoryFragments(prior)) == 0 && len(memoryFragments(fresh)) == 0 {
		out := mergeLegacyDigests(prior, fresh)
		if prior.ClaimsVersion > out.ClaimsVersion {
			out.ClaimsVersion = prior.ClaimsVersion
		}
		return out
	}
	out := fresh
	out.Fragments = mergeMemoryFragments(memoryFragments(prior), memoryFragments(fresh))
	if prior.ClaimsVersion > out.ClaimsVersion {
		out.ClaimsVersion = prior.ClaimsVersion
	}
	if out.HasMemoryClaims() && out.ClaimsVersion == 0 {
		out.ClaimsVersion = MemoryClaimsVersion
	}
	renderMemoryFragments(&out)
	return out
}

func mergeLegacyDigests(prior, fresh MemoryDigest) MemoryDigest {
	out := fresh
	if prior.Summary != "" && prior.Summary != fresh.Summary && !strings.Contains(strings.ToLower(fresh.Summary), strings.ToLower(prior.Summary)) {
		if fresh.Summary == "" {
			out.Summary = prior.Summary
		} else {
			out.Summary = prior.Summary + "\n\n" + fresh.Summary
		}
	}
	out.KeyFacts = dedupStrings(prior.KeyFacts, fresh.KeyFacts)
	if fresh.TasksAuthoritative {
		out.OpenTasks = dedupStrings(fresh.OpenTasks)
	} else {
		out.OpenTasks = dedupStrings(prior.OpenTasks, fresh.OpenTasks)
	}
	return out
}

func memoryFragments(d MemoryDigest) []MemoryFragment {
	if len(d.Fragments) > 0 {
		return append([]MemoryFragment(nil), d.Fragments...)
	}
	if d.SnapshotID == "" || (d.Summary == "" && len(d.KeyFacts) == 0 && len(d.OpenTasks) == 0) {
		return nil
	}
	return []MemoryFragment{{
		SourceSnapshot: d.SnapshotID, Summary: d.Summary, KeyFacts: d.KeyFacts,
		OpenTasks: d.OpenTasks, TasksAuthoritative: d.TasksAuthoritative,
	}}
}

func mergeMemoryFragments(groups ...[]MemoryFragment) []MemoryFragment {
	seen := map[string]bool{}
	var out []MemoryFragment
	for _, group := range groups {
		for _, fragment := range group {
			if fragment.SourceSnapshot == "" {
				continue
			}
			key := memoryFragmentKey(fragment)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, fragment)
		}
	}
	return out
}

func memoryFragmentKey(fragment MemoryFragment) string {
	// Include typed provenance; equal legacy text cannot erase distinct scopes.
	raw, _ := json.Marshal(fragment)
	return string(raw)
}

// MemoryProjection is a derived view. StateHash identifies its dependencies,
// not the address of a stored memory object.
type MemoryProjection struct {
	Digest    MemoryDigest
	Found     bool
	StateHash ContentHash
}
