package documentquery

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docViewBytes = 128 << 10
const docViewScanEvents = 512

func (queries *Queries) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	ctx = queries.readContext(ctx)
	if offset < -1 || limit < 0 || limit > 100 || (view != "chat" && view != "prompts" && view != "all" && view != "raw") {
		return domain.DocViewPage{}, domain.ErrValidation
	}
	if limit == 0 {
		limit = 20
	}
	return preparedDocViewRead(ctx, queries, repo, hash, base, func() (domain.DocViewPage, error) {
		return queries.readDocViewEvents(ctx, repo, hash, base, offset, limit, view)
	})
}

func (queries *Queries) readDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (response domain.DocViewPage, resultErr error) {
	ctx, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return domain.DocViewPage{}, err
	}
	defer func() { finishDocumentResponse(ctx, release, resultErr) }()
	ctx, err = queries.reauthorizeDocumentRead(ctx, repo)
	if err != nil {
		return domain.DocViewPage{}, err
	}
	return repositoryReadForRepo(ctx, queries, repo, func(read context.Context) (domain.DocViewPage, error) {
		out := domain.DocViewPage{Hash: hash, Next: -1, Events: []domain.DocViewEvent{}}
		source, err := queries.docViewSource(read, repo, hash, false)
		if err != nil {
			return out, err
		}
		out.Envelope, out.Total = source.envelope, source.total
		if source.root {
			out.DocIdentity = domain.DocumentIdentityRootV1
		}
		if base != "" {
			previous, err := queries.openDocViewBase(read, repo, base)
			if err != nil {
				return out, err
			}
			out.Inherited, err = queries.docViewBoundary(read, repo, hash, base, source, previous)
			if err != nil {
				return out, err
			}
		}
		if offset == -1 {
			offset = out.Inherited
		}
		if offset > source.total {
			return out, domain.ErrValidation
		}
		out.Offset = offset
		visible, bytes, position := 0, 0, offset
		for position < source.total && position-offset < docViewScanEvents && visible < limit {
			if err := read.Err(); err != nil {
				return out, err
			}
			entry, err := source.entry(position)
			if err != nil {
				return out, err
			}
			shown := view == "all" || view == "raw" || (entry.Kind == domain.EventMessage && (view == "chat" || (entry.Role == string(domain.RoleUser) && !entry.CompactSummary)))
			deferred := entry.Length > docViewBytes || (view != "raw" && (!shown || entry.Kind == domain.EventToolCall || entry.Kind == domain.EventToolResult || entry.Kind == domain.EventReasoning || entry.CompactSummary))
			event := domain.CIREvent{Kind: entry.Kind, Role: domain.Role(entry.Role), Seq: entry.Seq, ToolName: entry.ToolName, CompactSummary: entry.CompactSummary}
			cost := entry.Length + 128
			if deferred {
				metadata, err := json.Marshal(domain.DocViewEvent{Event: event, Index: position, Deferred: true, Bytes: entry.Length})
				if err != nil {
					return out, err
				}
				cost = len(metadata) + 1
			}
			if len(out.Events) > 0 && bytes+cost > docViewBytes {
				break
			}
			if !deferred {
				raw, err := source.read(entry.Offset, entry.Length)
				if err != nil {
					return out, err
				}
				event, err = domain.DecodeIndexedEvent(raw, entry.DocEventIndex)
				if err != nil {
					return out, err
				}
			}
			out.Events = append(out.Events, domain.DocViewEvent{Event: event, Index: position, Deferred: deferred, Bytes: entry.Length})
			bytes += cost
			if shown {
				visible++
			}
			position++
		}
		if position < source.total {
			out.Next = position
		}
		return out, nil
	})
}
