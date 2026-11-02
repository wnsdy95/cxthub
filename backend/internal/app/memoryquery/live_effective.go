package memoryquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type effectiveMemorySelectionState struct {
	view     domain.EffectiveMemoryPage
	source   domain.RepositoryViewSourceCheckpoint
	history  []domain.HistoryEvent
	evidence *liveEvidence
	digest   domain.MemoryDigest
}

func (queries *LiveQueries) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	request, err := NormalizeRequest(repo, request)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return readLiveMemory(ctx, queries, repo, func(current context.Context) (domain.EffectiveMemoryPage, error) {
		return queries.queryEffectiveMemory(current, repo, request)
	})
}

func (queries *LiveQueries) Project(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if queries == nil || queries.mode != LiveFrozen {
		return domain.EffectiveMemoryPage{}, domain.ErrCatalogUnsupported
	}
	request, err := NormalizeRequest(repo, request)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	if _, _, err := queries.memorySourceGeneration(ctx, repo); err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return queries.queryEffectiveMemory(ctx, repo, request)
}

func (queries *LiveQueries) SelectMemory(ctx context.Context, repo domain.ContentHash, selection domain.EffectiveMemorySelection) (domain.MemoryProjection, error) {
	if queries == nil || queries.mode != LiveFrozen {
		return domain.MemoryProjection{}, domain.ErrCatalogUnsupported
	}
	if domain.ValidateContentHash(repo) != nil || selection.Validate() != nil {
		return domain.MemoryProjection{}, domain.ErrValidation
	}
	if _, _, err := queries.memorySourceGeneration(ctx, repo); err != nil {
		return domain.MemoryProjection{}, err
	}
	return queries.selectMemory(ctx, repo, selection, nil)
}

func (queries *LiveQueries) queryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	selected, err := queries.prepareEffectiveMemorySelection(ctx, repo, request)
	out := selected.view
	if err != nil {
		return out, err
	}
	offset, err := Offset(request.Cursor, out.StateHash)
	if err != nil {
		return out, err
	}
	items, err := EffectiveItems(selected.digest, request.Content)
	if err != nil {
		return out, err
	}
	out.Total = len(items)
	if offset > len(items) {
		return out, domain.ErrValidation
	}
	out.DeliveryStateHash, err = queries.effectiveMemoryProof(ctx, repo, selected.source, out, selected.history, items, selected.evidence.contributions)
	if err != nil {
		return out, err
	}
	resolver, err := newMemoryIntegration(queries, selected.evidence, repo, selected.history)
	if err != nil {
		return out, err
	}
	return resolveEffectiveMemoryPage(ctx, resolver, out, items, offset, request.Limit)
}

func (queries *LiveQueries) prepareEffectiveMemorySelection(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (effectiveMemorySelectionState, error) {
	out := domain.EffectiveMemoryPage{Content: request.Content, Selection: request.Selection, Items: []domain.EffectiveMemoryItem{}}
	var selected effectiveMemorySelectionState
	evidence, err := queries.newLiveEvidence(ctx, repo, nil)
	if err != nil {
		return selected, err
	}
	out.Revision, err = queries.revisions.RepositoryRevision(ctx, repo)
	if err != nil {
		return selected, err
	}
	projection, err := queries.selectMemory(ctx, repo, request.Selection, evidence)
	if err != nil {
		return selected, err
	}
	digest := projection.Digest
	out.Inclusion, out.LineageHash = projection.Inclusion, projection.StateHash
	if evidence.contributions == nil {
		history, err := queries.history.ListHistoryEvents(ctx, repo)
		if err != nil {
			return selected, err
		}
		evidence.contributions = &liveContributionCache{repo: repo, history: history}
	}
	history := evidence.contributions.history
	sort.Slice(history, func(left, right int) bool { return history[left].ID < history[right].ID })
	historyBytes, err := json.Marshal(history)
	if err != nil {
		return selected, err
	}
	source, memorySource, err := queries.memorySourceGeneration(ctx, repo)
	if err != nil {
		return selected, err
	}
	basis := struct {
		Version          int
		Repo             domain.ContentHash
		Origin           string
		Selection        domain.EffectiveMemorySelection
		Lineage, History domain.ContentHash
		Graph, Evidence  uint64
		Content          string                                   `json:"Content,omitempty"`
		Source           *domain.RepositoryViewSourceCheckpoint   `json:"source,omitempty"`
		MemorySource     *domain.RepositoryMemorySourceGeneration `json:"memory_source,omitempty"`
	}{
		1, repo, evidence.origin, request.Selection, out.LineageHash, domain.HashContent(historyBytes), out.Revision.Graph, out.Revision.Evidence, request.Content, nil, nil,
	}
	if validLiveGraphGeneration(repo, source) {
		basis.Version, basis.Source = 2, &source
	}
	if memorySource != nil {
		basis.Version, basis.MemorySource = 3, memorySource
	}
	raw, err := json.Marshal(basis)
	if err != nil {
		return selected, err
	}
	out.StateHash = domain.HashContent(raw)
	return effectiveMemorySelectionState{view: out, source: source, history: history, evidence: evidence, digest: digest}, nil
}

func (queries *LiveQueries) selectMemory(ctx context.Context, repo domain.ContentHash, selection domain.EffectiveMemorySelection, evidence *liveEvidence) (domain.MemoryProjection, error) {
	var projection domain.MemoryProjection
	var err error
	switch {
	case selection.MemoryHash != "":
		if _, err = queries.metadata.GetSnapshot(ctx, repo, selection.SnapshotID); err != nil {
			return projection, err
		}
		projection.Digest, err = queries.projection.GetMemory(ctx, repo, selection.MemoryHash)
		if err != nil {
			return projection, err
		}
		projection.StateHash, err = domain.MemoryDigestHash(projection.Digest)
		if err != nil || projection.StateHash != selection.MemoryHash || projection.Digest.SnapshotID != selection.SnapshotID {
			return projection, domain.ErrIntegrity
		}
		projection.Found = true
	case selection.Branch != "":
		projection, err = queries.queryBranchMemory(ctx, repo, selection.Branch, selection.SnapshotID, selection.CodeCommit, evidence)
	default:
		projection, err = ProjectMemory(ctx, queries.projection, repo, selection.SnapshotID)
	}
	if err != nil {
		return projection, err
	}
	if err := (domain.MemoryDigest{ClaimsVersion: projection.Digest.ClaimsVersion}).ValidateMemoryClaims(); err != nil {
		return projection, fmt.Errorf("%w: stored memory version: %v", domain.ErrIntegrity, err)
	}
	for _, fragment := range projection.Digest.Fragments {
		part := domain.MemoryDigest{ClaimsVersion: projection.Digest.ClaimsVersion, Fragments: []domain.MemoryFragment{fragment}}
		if err := part.ValidateMemoryClaims(); err != nil {
			return projection, fmt.Errorf("%w: stored memory claims: %v", domain.ErrIntegrity, err)
		}
	}
	return projection, ctx.Err()
}

func resolveEffectiveMemoryPage(ctx context.Context, resolver *memoryIntegration, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem, offset, limit int) (domain.EffectiveMemoryPage, error) {
	used, next := 0, offset
	for next < len(items) && len(out.Items) < limit {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		item := items[next]
		if item.Kind == "code" {
			var err error
			item, err = resolver.assess(ctx, item, out.Selection.CodeCommit, out.Revision)
			if err != nil {
				return out, err
			}
			// A prior item must not consume this one's verification budget and
			// change its status compared with a one-item MCP page. Continue it
			// with a fresh read budget on the next request instead.
			if resolver.evidence.Limited() && len(out.Items) > 0 {
				break
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return out, err
		}
		if used+len(raw) > MaxPageBytes && len(out.Items) > 0 {
			break
		}
		if len(raw) > MaxPageBytes {
			return out, fmt.Errorf("%w: effective memory item exceeds page bound", domain.ErrValidation)
		}
		used += len(raw)
		out.Items = append(out.Items, item)
		next++
		if resolver.evidence.Limited() {
			break
		}
	}
	if next < len(items) {
		raw, _ := json.Marshal(Cursor{Version: 1, State: out.StateHash, Index: next})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
