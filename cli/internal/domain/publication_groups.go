package domain

import (
	"fmt"
	"reflect"
	"sort"
)

type publicationCatalog struct {
	byID         map[string]HistoryEvent
	accepted     map[string]bool
	all, pending []HistoryEvent
}

// Compare payloads before any overwrite or selection, including accepted duplicates.
func indexPublicationHistory(repo string, events, accepted []HistoryEvent) (publicationCatalog, error) {
	c := publicationCatalog{byID: map[string]HistoryEvent{}, accepted: map[string]bool{}}
	for _, batch := range [][]HistoryEvent{accepted, events} {
		for _, e := range batch {
			if e.RepoID != repo {
				return c, ErrHashMismatch
			}
			if err := ValidateHistoryEvent(e); err != nil {
				return c, err
			}
			if old, ok := c.byID[e.ID]; ok {
				if !reflect.DeepEqual(old, e) {
					return c, ErrHashMismatch
				}
			} else {
				c.byID[e.ID] = e
				c.all = append(c.all, e)
			}
		}
	}
	for _, e := range accepted {
		c.accepted[e.ID] = true
	}
	seen := map[string]bool{}
	for _, e := range events {
		if !c.accepted[e.ID] && !seen[e.ID] {
			c.pending = append(c.pending, e)
			seen[e.ID] = true
		}
	}
	return c, nil
}

// OrderHistoryPublications is the existing all-history completion policy, now pure.
// ancestry contains validated reachability (including self) for competing targets.
func OrderHistoryPublications(repo string, events, accepted []HistoryEvent, ancestry map[ContentHash]map[ContentHash]bool) ([]HistoryEvent, error) {
	c, err := indexPublicationHistory(repo, events, accepted)
	if err != nil {
		return nil, err
	}
	return c.order(repo, nil, ancestry)
}

// PublicationAncestryRoots identifies only competing completion targets. Loading
// single-source ancestry would add reads that the existing app preflight avoided.
func PublicationAncestryRoots(repo string, events, accepted []HistoryEvent) ([]ContentHash, error) {
	c, err := indexPublicationHistory(repo, events, accepted)
	if err != nil {
		return nil, err
	}
	return c.ancestryRoots(nil), nil
}

func (c publicationCatalog) ancestryRoots(selected map[string]bool) []ContentHash {
	groups := map[[2]string]map[ContentHash]bool{}
	for _, e := range c.pending {
		if e.Kind != "publish" || (selected != nil && !selected[e.BranchID]) {
			continue
		}
		key := [2]string{e.BranchID, e.GitAfter}
		if groups[key] == nil {
			groups[key] = map[ContentHash]bool{}
		}
		groups[key][e.Target] = true
	}
	needed := map[ContentHash]bool{}
	for _, group := range groups {
		if len(group) > 1 {
			for id := range group {
				needed[id] = true
			}
		}
	}
	roots := make([]ContentHash, 0, len(needed))
	for id := range needed {
		roots = append(roots, id)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	return roots
}

// Global name/revision ambiguity is checked BEFORE partitioning by identity.
// Accepted completion barriers never compete with later local captures.
func (catalog publicationCatalog) order(repoID string, selected map[string]bool, ancestry map[ContentHash]map[ContentHash]bool) ([]HistoryEvent, error) {
	ordinary := make([]HistoryEvent, 0, len(catalog.pending))
	groups := map[[3]string][]HistoryEvent{}
	var keys [][3]string
	identities := map[[3]string]string{}
	for _, e := range catalog.pending {
		if e.Kind != "publish" {
			if selected == nil || selected[e.BranchID] {
				ordinary = append(ordinary, e)
			}
			continue
		}
		if e.RepoID != repoID {
			return nil, ErrHashMismatch
		}
		if err := ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
		// A reused canonical name or tracking alias at this exact Git revision
		// must not let the first identity win before the other is uploaded.
		for _, name := range []string{e.Branch, e.LocalBranch} {
			if name == "" {
				continue
			}
			key := [3]string{e.RepoID, name, e.GitAfter}
			if old := identities[key]; old != "" && old != e.BranchID && (selected == nil || selected[old] || selected[e.BranchID]) {
				return nil, fmt.Errorf("%w: publication name %q at Git %s belongs to multiple branch identities", ErrSyncConflict, name, e.GitAfter)
			}
			if identities[key] == "" {
				identities[key] = e.BranchID
			}
		}
		if selected != nil && !selected[e.BranchID] {
			continue
		}
		// Names and worktrees are observations of an identity, not separate
		// groups. Preserve full Git object IDs, including SHA-256 repositories.
		key := [3]string{e.RepoID, e.BranchID, e.GitAfter}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], e)
	}
	observations := catalog.all
	for _, key := range keys {
		group := groups[key]
		ranks := map[ContentHash]int{}
		for _, e := range group {
			ranks[e.Target] = 0
		}
		var maximal ContentHash
		for target := range ranks {
			if len(ranks) == 1 {
				maximal = target
				break
			}
			ancestors, ok := ancestry[target]
			if !ok {
				return nil, fmt.Errorf("%w: missing publication ancestry for %s", ErrSyncConflict, target)
			}
			for other := range ranks {
				if ancestors[other] {
					ranks[target]++
				}
			}
			if ranks[target] == len(ranks) {
				maximal = target
			}
		}
		if maximal == "" {
			return nil, fmt.Errorf("%w: branch identity %s at Git %s has incomparable publications", ErrSyncConflict, key[1], key[2])
		}
		// An ancestor may expose an alias which the maximal publication cannot
		// satisfy. Refuse that ambiguity instead of allowing its worker to bind
		// the ancestor. Several publications of the maximal target may cover it.
		covered := map[string]bool{}
		names := map[string][]string{}
		for _, e := range group {
			names[e.ID] = PublicationSourceNames(e, observations)
			if e.Target == maximal {
				for _, name := range names[e.ID] {
					covered[name] = true
				}
			}
		}
		for _, e := range group {
			for _, name := range names[e.ID] {
				if !covered[name] {
					return nil, fmt.Errorf("%w: maximal publication for branch identity %s at Git %s does not cover source name %q", ErrSyncConflict, key[1], key[2], name)
				}
			}
		}
		// Every descendant has strictly more candidate ancestors in a DAG.
		// IDs only break ties between equivalent or already-covered sources.
		sort.Slice(group, func(i, j int) bool {
			if ranks[group[i].Target] != ranks[group[j].Target] {
				return ranks[group[i].Target] > ranks[group[j].Target]
			}
			return group[i].ID < group[j].ID
		})
		ordinary = append(ordinary, group...)
	}
	return ordinary, nil
}

// PublicationSourceNames follows the server ordinary-proof/alias contract.
func PublicationSourceNames(p HistoryEvent, events []HistoryEvent) []string {
	names := []string{p.Branch}
	if p.LocalBranch == "" || p.LocalBranch == p.Branch || p.WorktreeID == "" {
		return names
	}
	for _, proof := range events {
		if !publicationProof(p, proof) {
			continue
		}
		for _, attached := range events {
			if publicationAliasAttachment(p, proof, attached) {
				return append(names, p.LocalBranch)
			}
		}
	}
	return names
}

func publicationProof(p, e HistoryEvent) bool {
	return p.Kind == "publish" && e.Kind != "publish" && e.Kind != "pr-merge" &&
		e.RepoID == p.RepoID && e.BranchID == p.BranchID && e.Branch == p.Branch &&
		e.LocalBranch == p.LocalBranch && e.WorktreeID == p.WorktreeID && e.GitAfter == p.GitAfter && e.Target == p.Target
}

func publicationAliasAttachment(p, proof, a HistoryEvent) bool {
	return a.Kind == "attach" && a.RepoID == p.RepoID && a.BranchID == p.BranchID && a.LocalBranch != "" &&
		a.WorktreeID != "" && (a.WorktreeID == p.WorktreeID || a.LocalBranch == p.LocalBranch) && !a.CreatedAt.After(proof.CreatedAt)
}
