package http

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetDocumentFindFragments(query inbound.DocumentFindFragmentQuery) {
	server.docFindFragment = query
}

func (server *Server) getDocFindFragment(writer http.ResponseWriter, request *http.Request) {
	repo, hash := server.repoID(request), domain.ContentHash(request.PathValue("hash"))
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		server.respond(writer, nil, domain.ErrValidation)
		return
	}
	fragmentRequest, err := docFindFragmentRequest(query, repo, hash)
	if err != nil {
		server.respond(writer, nil, err)
		return
	}
	if server.docFindFragment == nil {
		server.writeError(writer, http.StatusServiceUnavailable, "unavailable", "document find fragments unavailable")
		return
	}
	bounded, release, ok := server.documentResponseRequest(writer, request)
	if !ok {
		return
	}
	defer release()
	ctx := inbound.WithDocumentReadAuthorizer(bounded.Context(), server.documentReadAuthorizer(request, repo))
	page, err := server.docFindFragment.ReadDocFindFragment(ctx, repo, hash, fragmentRequest)
	server.respond(writer, page, err)
}

func docFindFragmentRequest(query url.Values, repo, hash domain.ContentHash) (domain.DocFindFragmentRequest, error) {
	var request domain.DocFindFragmentRequest
	request.Legacy = query.Has("legacy")
	for key, values := range query {
		allowed := key == "offset" || key == "budget" || !request.Legacy && key == "selection" || request.Legacy && (key == "legacy" || key == "index" || key == "total")
		if !allowed || len(values) != 1 || values[0] == "" {
			return request, domain.ErrValidation
		}
	}
	if request.Legacy {
		if len(query) != 5 || query.Get("legacy") != "1" {
			return request, domain.ErrValidation
		}
	} else if len(query) != 3 {
		return request, domain.ErrValidation
	}
	request.Selection = query.Get("selection")
	fields := map[string]*int{"offset": &request.Offset, "budget": &request.Budget}
	if request.Legacy {
		fields["index"], fields["total"] = &request.Index, &request.Total
	}
	for key, target := range fields {
		raw := query.Get(key)
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || strconv.Itoa(value) != raw {
			return request, domain.ErrValidation
		}
		*target = value
	}
	if _, err := domain.DocFindFragmentSelection(repo, hash, request); err != nil {
		return request, err
	}
	return request, nil
}
