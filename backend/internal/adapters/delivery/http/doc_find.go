package http

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) SetDocumentFind(query inbound.DocumentFindQuery) {
	server.docFind = query
}

func (server *Server) getDocFindPage(writer http.ResponseWriter, request *http.Request) {
	accept := request.Header.Values("Accept")
	sourceRanges := len(accept) == 1 && accept[0] == "application/json, application/vnd.cxthub.find-source-ranges-v1+json"
	writer.Header().Add("Vary", "Accept")
	repo, hash := server.repoID(request), domain.ContentHash(request.PathValue("hash"))
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		server.respond(writer, nil, domain.ErrValidation)
		return
	}
	pageRequest, err := docFindPageRequest(query, repo, hash)
	if err != nil {
		server.respond(writer, nil, err)
		return
	}
	if server.docFind == nil {
		server.writeError(writer, http.StatusServiceUnavailable, "unavailable", "document find unavailable")
		return
	}
	bounded, release, ok := server.documentResponseRequest(writer, request)
	if !ok {
		return
	}
	defer release()
	ctx := inbound.WithDocumentReadAuthorizer(bounded.Context(), server.documentReadAuthorizer(request, repo))
	page, err := server.docFind.FindDocPage(ctx, repo, hash, pageRequest)
	if err == nil && !sourceRanges && pageRequest.Representation != domain.DocFindDisplayRepresentation && pageRequest.Representation != domain.DocFindPreparedRepresentation && len(page.Matches) > 0 {
		page.Matches = append([]domain.DocFindMatch{}, page.Matches...)
		for index := range page.Matches {
			page.Matches[index].SourceRange = nil
		}
	}
	server.respond(writer, page, err)
}

func docFindPageRequest(query url.Values, repo, hash domain.ContentHash) (domain.DocFindPageRequest, error) {
	if query.Get("legacy") != "" {
		return docLegacyFindPageRequest(query, repo, hash)
	}
	var request domain.DocFindPageRequest
	for key, values := range query {
		switch key {
		case "q", "selection", "from", "to", "cursor", "representation":
		default:
			return request, domain.ErrValidation
		}
		if len(values) != 1 || values[0] == "" {
			return request, domain.ErrValidation
		}
	}
	request.Query, request.Representation = query.Get("q"), query.Get("representation")
	keyCount := len(query)
	if request.Representation != "" {
		if request.Representation != domain.DocFindDisplayRepresentation && request.Representation != domain.DocFindPreparedRepresentation {
			return request, domain.ErrValidation
		}
		keyCount--
	}
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(hash) != nil || domain.ValidateDocFindQuery(request.Query) != nil {
		return request, domain.ErrValidation
	}
	if query.Has("cursor") {
		if keyCount != 2 {
			return request, domain.ErrValidation
		}
		request.Cursor = query.Get("cursor")
		selector, err := domain.DecodeDocFindSelector(request.Cursor)
		if err != nil || selector.Selection.Repo != repo || selector.Selection.Hash != hash || selector.QueryHash != domain.HashContent([]byte(request.Query)) || selector.Representation != request.Representation {
			return request, domain.ErrValidation
		}
		return request, nil
	}
	if keyCount != 4 {
		return request, domain.ErrValidation
	}
	request.Selection = query.Get("selection")
	selector, err := domain.DecodeDocVisibleSelector(request.Selection)
	if err != nil || selector.Purpose != "units" || selector.Repo != repo || selector.Hash != hash {
		return request, domain.ErrValidation
	}
	for key, target := range map[string]*int{"from": &request.From, "to": &request.To} {
		raw := query.Get(key)
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > selector.Total || strconv.Itoa(value) != raw {
			return request, domain.ErrValidation
		}
		*target = value
	}
	if request.From < selector.Start || request.From > request.To {
		return request, domain.ErrValidation
	}
	return request, nil
}
