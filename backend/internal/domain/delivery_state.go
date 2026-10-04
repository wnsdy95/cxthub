package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ContextDeliveryStateHash consumes the complete selected view, full merge
// semantics/history from the same read generation, and finalized publication
// bindings. Pagination, repository revisions and cursor hashes are deliberately
// absent. Only Snapshot.Branches is discarded from selected snapshot metadata:
// ancestry, overlays, immutable content and original provenance remain bound.
func ContextDeliveryStateHash(repo ContentHash, origin string, in ContextSelection, view ContextQueryView, bindings map[ContentHash][]CommitContextBinding) (ContentHash, error) {
	type source struct {
		Snapshot     Snapshot
		Publications []CommitContextBinding
	}
	sources := make([]source, 0, len(view.Snapshots))
	selected := make(map[ContentHash]bool, len(view.Snapshots))
	for _, snap := range view.Snapshots {
		selected[snap.ID] = true
		snap.Branches = nil // copy; never change the response or a cached snapshot
		pubs := append([]CommitContextBinding{}, bindings[snap.ID]...)
		sort.Slice(pubs, func(i, j int) bool { return pubs[i].EventID < pubs[j].EventID })
		sources = append(sources, source{snap, pubs})
	}
	wanted := map[string]bool{}
	if view.Inclusion != nil {
		for _, merge := range view.Inclusion.Merges {
			wanted[merge.EventID] = true
		}
	} else {
		for _, event := range view.History {
			if event.Kind == "pr-merge" && event.PRCompleted && (selected[event.Source] || selected[event.Target] || selected[event.SharedTarget]) {
				wanted[event.ID] = true
			}
		}
	}
	facts := make([]ContextMergeEvidence, 0)
	for _, fact := range view.Semantics.Merges {
		if wanted[fact.EventID] {
			facts = append(facts, fact)
			if fact.BirthID != "" {
				wanted[fact.BirthID] = true
			}
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].EventID < facts[j].EventID })
	receipts := make([]HistoryEvent, 0)
	for _, event := range view.History {
		if wanted[event.ID] {
			receipts = append(receipts, event)
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].ID < receipts[j].ID })
	scope, branch, code := in.Scope, in.Branch, in.CodeCommit
	if scope == "" {
		scope = "all"
	}
	if view.Branch != "" {
		branch = view.Branch
	}
	if view.Inclusion != nil {
		code = view.Inclusion.CodeCommit
	}
	// Resolved position/code replace aliases such as HEAD. Scope and branch
	// identity still distinguish separate selections of the same document.
	raw, err := json.Marshal(struct {
		Kind                              string
		Version                           int
		Repo                              ContentHash
		Origin, Scope, Branch, CodeCommit string
		Position                          ContentHash
		Inclusion                         *BranchContext
		Sources                           []source
		Facts                             []ContextMergeEvidence
		Receipts                          []HistoryEvent
	}{"context-delivery", 1, repo, origin, scope, branch, code, view.Position, view.Inclusion, sources, facts, receipts})
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

// EffectiveMemoryDeliveryStateHash requires all assessed items, in projection
// order, not a response page. LineageHash binds the selected memory graph and
// complete immutable digests, including text omitted by prompt excerpt limits.
// Newline-framed JSON records avoid another whole-projection allocation. The
// format is versioned independently of page/cursor hashes and repository clocks.
func EffectiveMemoryDeliveryStateHash(ctx context.Context, repo ContentHash, origin string, page EffectiveMemoryPage, complete []EffectiveMemoryItem) (ContentHash, error) {
	if len(complete) != page.Total {
		return "", ErrIntegrity
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	header := struct {
		Kind            string
		Version         int
		Repo            ContentHash
		Origin, Content string
		Selection       EffectiveMemorySelection
		Lineage         ContentHash
		Inclusion       *BranchContext
		Total           int
	}{"effective-memory-delivery", 1, repo, origin, page.Content, page.Selection, page.LineageHash, page.Inclusion, page.Total}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := enc.Encode(header); err != nil {
		return "", err
	}
	for _, item := range complete {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := enc.Encode(item); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return ContentHash("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}
