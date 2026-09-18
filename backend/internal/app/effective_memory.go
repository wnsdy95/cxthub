package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const effectiveMemoryPageBytes = 256 << 10
const effectiveMemoryMaxItems = 16384

type effectiveMemoryCursor struct {
	Version int                `json:"v"`
	State   domain.ContentHash `json:"state"`
	Index   int                `json:"index"`
}

// One read snapshot owns memory, accepted history and Git evidence. It never
// asks a provider to verify or enqueue anything, nor changes a worker's position.
func (s *Service) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if domain.ValidateContentHash(repo) != nil || in.Selection.Validate() != nil || in.Limit < 0 || in.Limit > 50 || len(in.Cursor) > 1024 {
		return domain.EffectiveMemoryPage{}, domain.ErrValidation
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.EffectiveMemoryPage, error) {
		return s.queryEffectiveMemory(ctx, repo, in)
	})
}
func (s *Service) queryEffectiveMemory(ctx context.Context, repo domain.ContentHash, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	out := domain.EffectiveMemoryPage{Selection: in.Selection, Items: []domain.EffectiveMemoryItem{}}
	evidence, err := s.newCodeEvidence(ctx, repo)
	if err != nil {
		return out, err
	}
	out.Revision, err = s.RepositoryRevision(ctx, repo)
	if err != nil {
		return out, err
	}
	var digest domain.MemoryDigest
	if in.Selection.MemoryHash != "" {
		if _, err = s.meta.GetSnapshot(ctx, repo, in.Selection.SnapshotID); err != nil {
			return out, err
		}
		digest, err = s.blobs.GetMemory(ctx, repo, in.Selection.MemoryHash)
		if err != nil {
			return out, err
		}
		actual, e := domain.MemoryDigestHash(digest)
		if e != nil || actual != in.Selection.MemoryHash || digest.SnapshotID != in.Selection.SnapshotID {
			return out, domain.ErrIntegrity
		}
		out.LineageHash = actual
	} else {
		projection, e := s.getMemoryProjection(ctx, repo, in.Selection.SnapshotID)
		if e != nil {
			return out, e
		}
		digest = projection.Digest
		out.LineageHash = projection.StateHash
	}
	if err := (domain.MemoryDigest{ClaimsVersion: digest.ClaimsVersion}).ValidateMemoryClaims(); err != nil {
		return out, fmt.Errorf("%w: stored memory version: %v", domain.ErrIntegrity, err)
	}
	for _, fragment := range digest.Fragments {
		part := domain.MemoryDigest{ClaimsVersion: digest.ClaimsVersion, Fragments: []domain.MemoryFragment{fragment}}
		if err := part.ValidateMemoryClaims(); err != nil {
			return out, fmt.Errorf("%w: stored memory claims: %v", domain.ErrIntegrity, err)
		}
	}
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return out, err
	}
	sort.Slice(history, func(i, j int) bool { return history[i].ID < history[j].ID })
	historyBytes, err := json.Marshal(history)
	if err != nil {
		return out, err
	}
	basis := struct {
		Version          int
		Repo             domain.ContentHash
		Origin           string
		Selection        domain.EffectiveMemorySelection
		Lineage, History domain.ContentHash
		Graph, Evidence  uint64
	}{
		1, repo, evidence.origin, in.Selection, out.LineageHash, domain.HashContent(historyBytes), out.Revision.Graph, out.Revision.Evidence,
	}
	raw, err := json.Marshal(basis)
	if err != nil {
		return out, err
	}
	out.StateHash = domain.HashContent(raw)
	offset := 0
	if in.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil {
			return out, domain.ErrValidation
		}
		var cursor effectiveMemoryCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Index < 0 {
			return out, domain.ErrValidation
		}
		if cursor.State != out.StateHash {
			return out, fmt.Errorf("%w: effective memory changed; restart without cursor", domain.ErrConflict)
		}
		offset = cursor.Index
	}
	items, err := effectiveMemoryItems(digest)
	if err != nil {
		return out, err
	}
	out.Total = len(items)
	if offset > len(items) {
		return out, domain.ErrValidation
	}
	resolver, err := newMemoryIntegration(s, evidence, repo, history)
	if err != nil {
		return out, err
	}
	return resolveEffectiveMemoryPage(ctx, resolver, out, items, offset, in.Limit)
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
			if resolver.evidence.limited && len(out.Items) > 0 {
				break
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return out, err
		}
		if used+len(raw) > effectiveMemoryPageBytes && len(out.Items) > 0 {
			break
		}
		if len(raw) > effectiveMemoryPageBytes {
			return out, fmt.Errorf("%w: effective memory item exceeds page bound", domain.ErrValidation)
		}
		used += len(raw)
		out.Items = append(out.Items, item)
		next++
		if resolver.evidence.limited {
			break
		}
	}
	if next < len(items) {
		raw, _ := json.Marshal(effectiveMemoryCursor{1, out.StateHash, next})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

func effectiveMemoryItems(d domain.MemoryDigest) ([]domain.EffectiveMemoryItem, error) {
	fragments := d.Fragments
	if len(fragments) == 0 {
		fragments = []domain.MemoryFragment{{SourceSnapshot: d.SnapshotID, Summary: d.Summary, KeyFacts: d.KeyFacts, OpenTasks: d.OpenTasks}}
	}
	out := []domain.EffectiveMemoryItem{}
	seen := map[domain.ContentHash]bool{}
	add := func(item domain.EffectiveMemoryItem) error {
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		item.ID = domain.HashContent(raw)
		if seen[item.ID] {
			return nil
		}
		seen[item.ID] = true
		if len(out) >= effectiveMemoryMaxItems {
			return fmt.Errorf("%w: memory is too large; select a narrower context or stored object", domain.ErrValidation)
		}
		out = append(out, item)
		return nil
	}
	for _, fragment := range fragments {
		for _, claim := range fragment.Claims {
			state := domain.AssessMemoryClaim(claim, nil, "unknown")
			if err := add(domain.EffectiveMemoryItem{SourceSnapshot: fragment.SourceSnapshot, Kind: claim.Kind, Text: claim.Text, Code: claim.Code, MemoryClaimAssessment: state}); err != nil {
				return nil, err
			}
		}
		historical := []struct {
			kind  string
			texts []string
		}{{"legacy_summary", []string{fragment.Summary}}, {"legacy_fact", fragment.KeyFacts}, {"legacy_task", fragment.OpenTasks}}
		for _, section := range historical {
			for _, text := range section.texts {
				if !utf8.ValidString(text) {
					return nil, domain.ErrIntegrity
				}
				originalHash := domain.HashContent([]byte(text))
				part := 0
				for len(text) > 0 {
					n := len(text)
					if n > 8192 {
						n = 8192
						for n > 0 && !utf8.RuneStart(text[n]) {
							n--
						}
					}
					item := domain.EffectiveMemoryItem{SourceSnapshot: fragment.SourceSnapshot, Kind: section.kind, Text: text[:n], TextHash: originalHash, Part: part, MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "untyped_historical_text"}}
					if err := add(item); err != nil {
						return nil, err
					}
					text = text[n:]
					part++
				}
			}
		}
	}
	return out, nil
}

// An author's malformed comparison parent is reviewable evidence, not a reason
// to hide every other contribution in the selected memory.
func isMemoryScopeError(err error) bool { return errors.Is(err, domain.ErrValidation) }
