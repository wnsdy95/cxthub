package documentquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.ConversationFindQuery = (*Queries)(nil)

func (queries *Queries) QueryConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
	return queries.conversationFind(outbound.WithDocReadOnly(ctx), repo, hash, request)
}

func (queries *Queries) PrepareConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
	if queries.readOnly || outbound.DocReadOnly(ctx) {
		return domain.ErrForbidden
	}
	_, err := queries.conversationFind(ctx, repo, hash, request)
	var pending *domain.ConversationFindNotReady
	if errors.As(err, &pending) && domain.IsOnlyConversationFindPending(pending.Cause, false) {
		return nil
	}
	return err
}

func (queries *Queries) conversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
	if validateHashes(repo, hash) != nil || request.Validate() != nil {
		return domain.ConversationFindPage{}, domain.ErrValidation
	}
	if request.Cursor != "" {
		find := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: request.Query, Cursor: request.Cursor}
		if _, err := docFindSelection(repo, hash, find); err != nil {
			find.Legacy = true
			if _, err := domain.DocLegacyFindSelection(repo, hash, find); err != nil {
				return domain.ConversationFindPage{}, domain.ErrValidation
			}
		}
	}
	identity, err := queries.prepareDocVisibleSource(ctx, repo, hash, docRangeLegacyBytes)
	if err != nil {
		return domain.ConversationFindPage{}, conversationFindFailure(err, request.Cursor, outbound.DocReadOnly(ctx))
	}
	root := identity == domain.DocumentIdentityRootV1
	find := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: request.Query, Cursor: request.Cursor, Legacy: !root}
	if find.Cursor == "" {
		find.Cursor, err = queries.conversationFindSelection(ctx, repo, hash, request.Query, root)
		if err != nil {
			return domain.ConversationFindPage{}, conversationFindFailure(err, "", outbound.DocReadOnly(ctx))
		}
	}
	page, err := queries.FindDocPage(ctx, repo, hash, find)
	if err != nil {
		return domain.ConversationFindPage{}, conversationFindFailure(err, find.Cursor, outbound.DocReadOnly(ctx))
	}
	return domain.ConversationFindPage{Page: page, ReplayCursor: find.Cursor}, nil
}

func conversationFindFailure(err error, cursor string, readOnly bool) error {
	if domain.IsOnlyConversationFindPending(err, readOnly) {
		return &domain.ConversationFindNotReady{Cursor: cursor, Cause: err}
	}
	return err
}

func (queries *Queries) conversationFindSelection(ctx context.Context, repo, hash domain.ContentHash, query string, root bool) (string, error) {
	var cursor string
	err := queries.interactiveViewWork(ctx, func(admitted context.Context) error {
		authorized, err := queries.authorizeVisibleRead(admitted, repo)
		if err != nil {
			return err
		}
		cursor, err = repositoryReadForRepo(authorized, queries, repo, func(read context.Context) (string, error) {
			if !root {
				source, available, err := queries.legacyGenerationSource(read, repo, hash)
				if err != nil {
					return "", err
				}
				if !available {
					return "", domain.ErrDocRangeIndexUnavailable
				}
				selector, err := domain.DocLegacyFindSelection(repo, hash, domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: query, Legacy: true, Total: source.total, To: source.total})
				if err != nil {
					return "", err
				}
				return selector.Encode()
			}
			selection, _, err := visiblePageSelection(repo, hash, domain.DocVisiblePageRequest{View: "all", Limit: 1})
			if err != nil {
				return "", err
			}
			opened, err := queries.openDocVisibleSelection(read, selection, false)
			if err != nil {
				return "", err
			}
			selector := domain.DocFindSelector{Version: 3, Representation: domain.DocFindPreparedRepresentation, Selection: opened.selection, QueryHash: domain.HashContent([]byte(query)), To: opened.source.total, Prepared: &domain.DocFindPreparedProgress{}}
			return selector.Encode()
		})
		return err
	})
	return cursor, err
}
