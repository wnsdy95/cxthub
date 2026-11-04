package documentquery

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	ctx = queries.readContext(ctx)
	if index < 0 || offset < 0 || budget < 4 || budget > 64<<10 {
		return domain.DocViewFragment{}, domain.ErrValidation
	}
	return preparedDocViewRead(ctx, queries, repo, hash, "", func() (domain.DocViewFragment, error) {
		return queries.readDocViewFragment(ctx, repo, hash, index, offset, budget)
	})
}

func (queries *Queries) readDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (response domain.DocViewFragment, resultErr error) {
	ctx, release, err := queries.acquireViewInteractive(ctx)
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	defer func() { finishDocumentResponse(ctx, release, resultErr) }()
	ctx, err = queries.reauthorizeDocumentRead(ctx, repo)
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	return repositoryReadForRepo(ctx, queries, repo, func(read context.Context) (domain.DocViewFragment, error) {
		source, err := queries.docViewSource(read, repo, hash, false)
		if err != nil {
			return domain.DocViewFragment{}, err
		}
		if index >= source.total {
			return domain.DocViewFragment{}, domain.ErrValidation
		}
		entry, err := source.location(index)
		if err != nil {
			return domain.DocViewFragment{}, err
		}
		return readDocViewFragmentRange(source, entry, offset, budget)
	})
}

func readDocViewFragmentRange(source docViewSource, entry domain.DocEventIndex, offset, budget int) (domain.DocViewFragment, error) {
	if offset < 0 || offset >= entry.Length || budget < 4 || budget > 64<<10 {
		return domain.DocViewFragment{}, domain.ErrValidation
	}
	if entry.Offset < 0 || entry.Length > domain.MaxConversationDocumentBytes || entry.Offset > domain.MaxConversationDocumentBytes-entry.Length || domain.ValidateContentHash(entry.Hash) != nil {
		return domain.DocViewFragment{}, domain.ErrIntegrity
	}
	length := min(budget, entry.Length-offset)
	raw, err := source.read(entry.Offset+offset, length)
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	if len(raw) != length {
		return domain.DocViewFragment{}, domain.ErrIntegrity
	}
	if !utf8.RuneStart(raw[0]) {
		return domain.DocViewFragment{}, domain.ErrValidation
	}
	if !utf8.Valid(raw) {
		boundary := len(raw) - 1
		for boundary > 0 && len(raw)-boundary < utf8.UTFMax && !utf8.RuneStart(raw[boundary]) {
			boundary--
		}
		if offset+len(raw) == entry.Length || utf8.FullRune(raw[boundary:]) || !utf8.Valid(raw[:boundary]) {
			return domain.DocViewFragment{}, fmt.Errorf("%w: invalid event UTF-8", domain.ErrIntegrity)
		}
		raw = raw[:boundary]
	}
	if len(raw) == 0 || offset == 0 && len(raw) == entry.Length && domain.HashContent(raw) != entry.Hash {
		return domain.DocViewFragment{}, domain.ErrIntegrity
	}
	next := offset + len(raw)
	if next == entry.Length {
		next = -1
	}
	return domain.DocViewFragment{Index: entry.Index, Offset: offset, Next: next, Total: entry.Length, JSON: string(raw)}, nil
}
