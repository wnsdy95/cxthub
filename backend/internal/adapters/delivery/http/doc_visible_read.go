package http

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) getDocVisiblePage(w http.ResponseWriter, r *http.Request, query url.Values) {
	repo, hash := s.repoID(r), domain.ContentHash(r.PathValue("hash"))
	pageRequest, err := docVisiblePageRequest(query, repo, hash)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	viewer := s.documentViews
	if viewer == nil {
		s.respond(w, nil, domain.ErrDocVisibleProjectionUnsupported)
		return
	}
	request, release, ok := s.documentResponseRequest(w, r)
	if !ok {
		return
	}
	defer release()
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), s.documentReadAuthorizer(r, repo))
	out, err := viewer.ReadDocVisiblePage(ctx, repo, hash, pageRequest)
	s.respond(w, out, err)
}

func (s *Server) docVisibleReadAuthorizer(r *http.Request, repo domain.ContentHash) inbound.DocumentReadAuthorization {
	token := s.requestToken(r)
	return func(ctx context.Context, requestedRepo domain.ContentHash) (context.Context, error) {
		ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, struct{}{}), "")
		if requestedRepo != repo {
			return ctx, &httpAccessError{status: http.StatusForbidden, code: "forbidden", cause: domain.ErrForbidden,
				message: "document read repository does not match request"}
		}
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		if token != "" {
			if user, err := s.id.ResolveUser(ctx, token); err == nil {
				ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, user), user.ID)
			}
		}
		return ctx, s.checkRepoRole(ctx, repo, domain.RoleViewer)
	}
}

func docVisiblePageRequest(query url.Values, repo, hash domain.ContentHash) (domain.DocVisiblePageRequest, error) {
	var request domain.DocVisiblePageRequest
	for key, values := range query {
		switch key {
		case "projection", "view", "offset", "limit", "base", "cursor", "expand":
		default:
			return request, domain.ErrValidation
		}
		if len(values) != 1 || values[0] == "" {
			return request, domain.ErrValidation
		}
	}
	if query.Get("projection") != domain.DocVisibleProjection || domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(hash) != nil {
		return request, domain.ErrValidation
	}
	if query.Has("cursor") || query.Has("expand") {
		if len(query) != 2 {
			return request, domain.ErrValidation
		}
		request.Cursor, request.Expand = query.Get("cursor"), query.Get("expand")
		encoded, purpose := request.Cursor, "units"
		if request.Expand != "" {
			encoded, purpose = request.Expand, "fold"
		}
		selector, err := domain.DecodeDocVisibleSelector(encoded)
		if err != nil || selector.Purpose != purpose || selector.Repo != repo || selector.Hash != hash {
			return request, domain.ErrValidation
		}
		return request, nil
	}
	request.View, request.Base = query.Get("view"), domain.ContentHash(query.Get("base"))
	if request.View != "all" && request.View != "chat" && request.View != "prompts" || domain.ValidateOptionalContentHash(request.Base) != nil {
		return request, domain.ErrValidation
	}
	request.Offset, request.Limit = -1, 20
	for key, target := range map[string]*int{"offset": &request.Offset, "limit": &request.Limit} {
		if raw := query.Get(key); raw != "" {
			if raw != "-1" && strings.Trim(raw, "0123456789") != "" {
				return request, domain.ErrValidation
			}
			value, err := strconv.Atoi(raw)
			if err != nil {
				return request, domain.ErrValidation
			}
			*target = value
		}
	}
	if request.Offset < -1 || request.Offset > domain.MaxConversationDocumentBytes || request.Limit < 1 || request.Limit > 100 {
		return request, domain.ErrValidation
	}
	return request, nil
}
