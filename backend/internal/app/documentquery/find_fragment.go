package documentquery

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.DocumentFindFragmentQuery = (*Queries)(nil)

func (queries *Queries) ReadDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error) {
	ctx = queries.readContext(ctx)
	selection, err := domain.DocFindFragmentSelection(repo, hash, request)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	if request.Legacy {
		return queries.readLegacyDocFindFragment(ctx, repo, hash, request)
	}
	return preparedDocVisibleRead(ctx, queries, repo, hash, selection.Base, func() (domain.DocFindFragmentPage, error) {
		return queries.readRootDocFindFragment(ctx, selection, request)
	})
}

func (queries *Queries) readRootDocFindFragment(ctx context.Context, selection domain.DocVisibleSelector, request domain.DocFindFragmentRequest) (response domain.DocFindFragmentPage, resultErr error) {
	ctx, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	defer func() { finishDocumentResponse(ctx, release, resultErr) }()
	ctx, err = queries.authorizeVisibleRead(ctx, selection.Repo)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	return repositoryReadForRepo(ctx, queries, selection.Repo, func(read context.Context) (domain.DocFindFragmentPage, error) {
		opened, err := queries.openDocVisibleSelection(read, selection, true)
		if err != nil {
			return domain.DocFindFragmentPage{}, err
		}
		unit, position, err := opened.units.Unit(read, "all", selection.Next, true)
		if err != nil {
			return domain.DocFindFragmentPage{}, err
		}
		if unit.Validate(opened.source.total, "all") != nil || unit.EventIndex != selection.Next {
			return domain.DocFindFragmentPage{}, domain.ErrIntegrity
		}
		if position != selection.Unit {
			return domain.DocFindFragmentPage{}, domain.ErrValidation
		}
		return readDocFindSourceFragment(opened.source, selection.Hash, opened.selection.Generation, selection.Next, request.Offset, request.Budget)
	})
}

func (queries *Queries) readLegacyDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (response domain.DocFindFragmentPage, resultErr error) {
	ctx, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	defer func() { finishDocumentResponse(ctx, release, resultErr) }()
	ctx, err = queries.authorizeVisibleRead(ctx, repo)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	return repositoryReadForRepo(ctx, queries, repo, func(read context.Context) (domain.DocFindFragmentPage, error) {
		snapshot, err := queries.metadata.GetSnapshot(read, repo, hash)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.DocFindFragmentPage{}, err
		}
		if err == nil {
			if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
				return domain.DocFindFragmentPage{}, domain.ErrDocVisibleProjectionUnsupported
			}
			if err := matchReadSnapshot(domain.DocumentRef{Hash: hash}, snapshot); err != nil {
				return domain.DocFindFragmentPage{}, err
			}
		}
		source, err := queries.legacyDocFindFragmentSource(outbound.WithDocReadOnly(read), repo, hash)
		if err != nil {
			return domain.DocFindFragmentPage{}, err
		}
		if source.root {
			return domain.DocFindFragmentPage{}, domain.ErrDocVisibleProjectionUnsupported
		}
		if source.total != request.Total {
			return domain.DocFindFragmentPage{}, domain.ErrIntegrity
		}
		return readDocFindSourceFragment(source, hash, "", request.Index, request.Offset, request.Budget)
	})
}

func (queries *Queries) legacyDocFindFragmentSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	if queries.index != nil {
		return queries.legacyFindSource(ctx, repo, hash)
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

func readDocFindSourceFragment(source docViewSource, hash, generation domain.ContentHash, index, offset, budget int) (domain.DocFindFragmentPage, error) {
	if index < 0 || index >= source.total {
		return domain.DocFindFragmentPage{}, domain.ErrValidation
	}
	entry, err := source.location(index)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	if entry.Index != index {
		return domain.DocFindFragmentPage{}, domain.ErrIntegrity
	}
	fragment, err := readDocViewFragmentRange(source, entry, offset, budget)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	previous := -1
	if offset > 0 {
		previous = max(0, offset-budget)
		probeLength := min(utf8.UTFMax, entry.Length-previous)
		probe, err := source.read(entry.Offset+previous, probeLength)
		if err != nil {
			return domain.DocFindFragmentPage{}, err
		}
		if len(probe) != probeLength {
			return domain.DocFindFragmentPage{}, domain.ErrIntegrity
		}
		boundary := 0
		for boundary < len(probe) && !utf8.RuneStart(probe[boundary]) {
			boundary++
		}
		if boundary == len(probe) || previous+boundary >= offset {
			return domain.DocFindFragmentPage{}, domain.ErrIntegrity
		}
		if decoded, size := utf8.DecodeRune(probe[boundary:]); utf8.FullRune(probe[boundary:]) && decoded == utf8.RuneError && size == 1 {
			return domain.DocFindFragmentPage{}, domain.ErrIntegrity
		}
		previous += boundary
	}
	projection := domain.DocFindFragmentProjection
	if !source.root {
		projection = domain.DocLegacyFindFragmentProjection
	}
	return domain.DocFindFragmentPage{Projection: projection, Hash: hash, Generation: generation,
		Index: index, Offset: offset, Total: fragment.Total, JSON: fragment.JSON, Next: fragment.Next, Previous: previous}, nil
}
