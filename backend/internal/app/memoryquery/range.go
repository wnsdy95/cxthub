package memoryquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func ReadPage(ctx context.Context, reader outbound.MemoryReadRangeStore, repo, identity domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	var empty domain.EffectiveMemoryPage
	request, err := NormalizeRequest(repo, request)
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if reader == nil {
		return empty, domain.ErrCatalogUnsupported
	}
	if domain.ValidateContentHash(identity) != nil {
		return empty, domain.ErrValidation
	}
	var out domain.EffectiveMemoryPage
	err = reader.WithMemoryReadRange(ctx, repo, identity, func(bound context.Context, header domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
		if err := bound.Err(); err != nil {
			return err
		}
		if header.Source.Graph.RepoID != repo {
			return domain.ErrIntegrity
		}
		if header.View.Selection != request.Selection || header.View.Content != request.Content {
			return domain.ErrValidation
		}
		out = header.View
		out.Items = []domain.EffectiveMemoryItem{}
		offset, err := Offset(request.Cursor, out.StateHash)
		if err != nil {
			return err
		}
		if offset > out.Total {
			return domain.ErrValidation
		}
		ordinal := sort.Search(len(header.Chunks), func(index int) bool { return header.Chunks[index].Start+header.Chunks[index].Count > offset })
		next, used := offset, 0
	pages:
		for ordinal < len(header.Chunks) && len(out.Items) < request.Limit && used < MaxPageBytes {
			if err := bound.Err(); err != nil {
				return err
			}
			items, err := chunks.ReadChunk(bound, ordinal)
			if err != nil {
				return err
			}
			for _, item := range items[next-header.Chunks[ordinal].Start:] {
				if len(out.Items) == request.Limit {
					break pages
				}
				if err := bound.Err(); err != nil {
					return err
				}
				raw, err := json.Marshal(item)
				if err != nil {
					return err
				}
				if len(raw) > MaxPageBytes {
					return domain.ErrMemoryProjectionLimit
				}
				if used+len(raw) > MaxPageBytes {
					break pages
				}
				out.Items = append(out.Items, item)
				used += len(raw)
				next++
			}
			ordinal++
		}
		if next < out.Total {
			raw, err := json.Marshal(Cursor{Version: 1, State: out.StateHash, Index: next})
			if err != nil {
				return err
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		return bound.Err()
	})
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return out, nil
}
