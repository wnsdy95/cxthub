package archivequery

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

const (
	searchDefaultLimit = 50
	searchMaxLimit     = 200
	searchSnippetRunes = 120
)

// Search reads metadata and indexed readable events. Identical inherited events
// are attributed to their oldest snapshot; archival tool bodies are not searched.
func (queries *ArchiveQueries) Search(ctx context.Context, input inbound.SearchInput) (inbound.SearchOutput, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadDocument(ctx, input.RepoID, read)
	}, func(bound context.Context) (inbound.SearchOutput, error) {
		query := strings.ToLower(strings.TrimSpace(input.Query))
		if len([]rune(query)) < 2 || len([]rune(query)) > 256 {
			return inbound.SearchOutput{}, fmt.Errorf("%w: Search term must contain 2 to 256 characters", domain.ErrValidation)
		}
		limit := input.Limit
		if limit <= 0 || limit > searchMaxLimit {
			limit = searchDefaultLimit
		}
		snapshots, err := queries.metadata.ListSnapshots(bound, input.RepoID, "")
		if err != nil {
			return inbound.SearchOutput{}, err
		}
		sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].CreatedAt.Before(snapshots[right].CreatedAt) })
		output := inbound.SearchOutput{Hits: []inbound.SearchHit{}}
		add := func(hit inbound.SearchHit) bool {
			if len(output.Hits) >= limit {
				output.Truncated = true
				return false
			}
			output.Hits = append(output.Hits, hit)
			return true
		}
		for _, snapshot := range snapshots {
			if !strings.Contains(strings.ToLower(snapshot.Message), query) && !strings.Contains(strings.ToLower(snapshot.Author.Name), query) {
				continue
			}
			if !add(inbound.SearchHit{
				SnapshotID: snapshot.ID, Branch: snapshot.Branch, Kind: "commit",
				Snippet: SearchSnippet(snapshot.Message, query), CreatedAt: snapshot.CreatedAt.UTC().Format(time.RFC3339),
			}) {
				return output, nil
			}
		}
		candidates, err := queries.matchingDocHashes(bound, input.RepoID, query)
		if err != nil {
			return output, err
		}
		documentsSeen := map[domain.DocumentRef]bool{}
		eventsSeen := map[string]bool{}
		for _, snapshot := range snapshots {
			if snapshot.DocHash == "" || documentsSeen[snapshot.DocumentRef()] || (snapshot.DocIdentity == domain.DocumentIdentityLegacy && candidates != nil && !candidates[snapshot.DocHash]) {
				continue
			}
			documentsSeen[snapshot.DocumentRef()] = true
			var root *documentquery.SearchIndex
			if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
				root, err = queries.documents.RootSearchIndex(bound, input.RepoID, snapshot.DocHash)
				if err != nil {
					return output, err
				}
				if root == nil {
					return output, domain.ErrIntegrity
				}
			}
			after := -1
			for {
				var hits []domain.DocEventIndex
				if root != nil {
					hits, err = root.Search(bound, query, after, 200)
				} else {
					hits, err = queries.documents.SearchDocEvents(bound, input.RepoID, snapshot.DocHash, query, after, 200)
				}
				if err != nil {
					return output, err
				}
				for _, hit := range hits {
					after = hit.Index
					key := string(hit.Hash)
					if eventsSeen[key] {
						continue
					}
					eventsSeen[key] = true
					eventIndex := hit.Index
					if !add(inbound.SearchHit{SnapshotID: snapshot.ID, Branch: snapshot.Branch, Kind: "event", Role: hit.Role, Seq: hit.Seq, EventIndex: &eventIndex, Snippet: SearchSnippet(hit.Text, query), CreatedAt: snapshot.CreatedAt.UTC().Format(time.RFC3339)}) {
						return output, nil
					}
				}
				if len(hits) < 200 {
					break
				}
			}
		}
		return output, nil
	})
}

// A nil candidate set means the adapter searches each indexed document. An empty
// non-nil set excludes legacy indexed documents only; roots still require their
// explicit current-byte reader before searching the verified projection.
func (queries *ArchiveQueries) MatchingDocHashes(ctx context.Context, repo domain.ContentHash, query string) (map[domain.ContentHash]bool, error) {
	return scopedRead(queries, func(read func(context.Context) error) error {
		return queries.scope.ReadDocument(ctx, repo, read)
	}, func(bound context.Context) (map[domain.ContentHash]bool, error) {
		return queries.matchingDocHashes(bound, repo, query)
	})
}

func (queries *ArchiveQueries) matchingDocHashes(ctx context.Context, repo domain.ContentHash, query string) (map[domain.ContentHash]bool, error) {
	if queries.candidates != nil {
		return queries.candidates.MatchingDocHashes(ctx, repo, strings.ToLower(strings.TrimSpace(query)))
	}
	return nil, nil
}

// searchSnippet safely truncates the first match to a single line.
func SearchSnippet(text, query string) string {
	text = strings.Join(strings.Fields(text), " ")
	byteIndex := strings.Index(strings.ToLower(text), query)
	if byteIndex < 0 {
		byteIndex = 0
	}
	runes := []rune(text)
	start := len([]rune(text[:byteIndex])) - 40
	if start < 0 {
		start = 0
	}
	end := start + searchSnippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	output := string(runes[start:end])
	if start > 0 {
		output = "…" + output
	}
	if end < len(runes) {
		output += "…"
	}
	return output
}
