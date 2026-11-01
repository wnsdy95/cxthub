package documentquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (queries *Queries) findLegacyDocPage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindPage, error) {
	selector, err := domain.DocLegacyFindSelection(repo, hash, request)
	if err != nil {
		return domain.DocFindPage{}, err
	}
	prepare := func(prepare context.Context, repo, target domain.ContentHash) (bool, error) {
		if request.Representation == domain.DocFindPreparedRepresentation {
			requirement := docRangeLegacyBytes
			if target != hash {
				requirement = docRangeLegacyMetadata
			}
			_, err := queries.prepareDocVisibleSource(prepare, repo, target, requirement)
			return true, err
		}
		if target == hash {
			return false, nil
		}
		return queries.prepareDocViewSource(prepare, repo, target, docRangeRootOnly)
	}
	return preparedDocumentProjectionRead(ctx, queries, repo, hash, selector.Base, prepare, func() (response domain.DocFindPage, resultErr error) {
		admitted, release, err := queries.acquireViewBulk(ctx)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		defer func() { finishDocumentResponse(admitted, release, resultErr) }()
		authorized, err := queries.authorizeVisibleRead(admitted, repo)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		return repositoryReadForRepo(authorized, queries, repo, func(read context.Context) (domain.DocFindPage, error) {
			snapshot, err := queries.metadata.GetSnapshot(read, repo, hash)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return domain.DocFindPage{}, err
			}
			if err == nil && snapshot.DocIdentity != domain.DocumentIdentityLegacy {
				return domain.DocFindPage{}, domain.ErrDocVisibleProjectionUnsupported
			}
			var source docViewSource
			if request.Representation == domain.DocFindPreparedRepresentation {
				var available bool
				source, available, err = queries.legacyGenerationSource(read, repo, hash)
				if _, enabled := queries.docBytePreparation(); err == nil && available && enabled && source.chunk == nil && source.streamBytes > 0 {
					err = errDocViewGenerationNeedsPreparation
				}
				if err == nil && !available {
					err = errDocViewGenerationNeedsPreparation
				}
				source.findFields = nil
			} else {
				source, err = queries.legacyFindSource(outbound.WithDocReadOnly(read), repo, hash)
			}
			if err != nil {
				return domain.DocFindPage{}, err
			}
			if source.root {
				return domain.DocFindPage{}, domain.ErrDocVisibleProjectionUnsupported
			}
			source = queries.withPreparedDocFindSource(read, repo, hash, source)
			inherited := 0
			if selector.Base != "" {
				var base docViewSource
				if request.Representation == domain.DocFindPreparedRepresentation {
					base, err = queries.openDocViewBase(read, repo, selector.Base)
				} else {
					base, err = queries.legacyFindSource(outbound.WithDocReadOnly(read), repo, selector.Base)
				}
				if err != nil {
					return domain.DocFindPage{}, err
				}
				inherited, err = queries.docViewBoundary(read, repo, hash, selector.Base, source, base)
				if err != nil {
					return domain.DocFindPage{}, err
				}
			}
			if source.total != selector.Total || inherited != selector.Inherited {
				return domain.DocFindPage{}, domain.ErrIntegrity
			}
			position := docFindProgress{From: selector.From, To: selector.To, Index: selector.Index, Field: selector.Field, Byte: selector.Byte, Representation: selector.Representation}
			if selector.Prepared != nil {
				position.Prepared = *selector.Prepared
			}
			page, err := scanDocFindRange(read, source, &position, request.Query)
			if err != nil {
				return domain.DocFindPage{}, err
			}
			page.Projection, page.Hash = domain.DocLegacyFindProjection, hash
			if selector.Representation == domain.DocFindDisplayRepresentation {
				page.Projection = domain.DocLegacyFindDisplayProjection
			}
			if selector.Representation == domain.DocFindPreparedRepresentation {
				page.Projection = domain.DocLegacyFindPreparedProjection
				selector.Prepared = &position.Prepared
			}
			if position.Index < position.To {
				selector.Index, selector.Field, selector.Byte = position.Index, position.Field, position.Byte
				next, err := selector.Encode()
				if err != nil {
					return domain.DocFindPage{}, err
				}
				page.NextCursor = &next
			}
			return page, nil
		})
	})
}

func (queries *Queries) legacyFindSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	if source, available, err := queries.legacyGenerationSource(ctx, repo, hash); available || err != nil {
		return source, err
	}
	source, err := queries.docViewSource(ctx, repo, hash, true)
	if !errors.Is(err, domain.ErrAgentHistoryUnavailable) {
		return source, err
	}
	doc, err := queries.legacy.GetDoc(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	if doc.DocumentRef() != (domain.DocumentRef{Hash: hash}) {
		return docViewSource{}, domain.ErrUnsupportedDocumentIdentity
	}
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		return docViewSource{}, err
	}
	plan, err := verified.PlanReadIndexContext(ctx)
	if err != nil {
		return docViewSource{}, err
	}
	index, err := plan.BuildMetadataContext(ctx)
	if err != nil {
		return docViewSource{}, err
	}
	return legacyDocViewSource(docReadSource{index: index, read: func(offset, length int) ([]byte, error) {
		return verified.EventStreamRange(ctx, offset, length)
	}}), nil
}
