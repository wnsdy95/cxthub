package http

import (
	"net/http"
	"net/url"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) documentReadAuthorizer(request *http.Request, repo domain.ContentHash) inbound.DocumentReadAuthorization {
	if server.documentQueryProfile == inbound.DocumentQueryVerifiedRoots {
		return server.memoryReadAuthorizer(request, repo)
	}
	return server.docVisibleReadAuthorizer(request, repo)
}

func docReadQuery(r *http.Request, keys ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, domain.ErrValidation
	}
	for _, key := range keys {
		if len(query[key]) > 1 {
			return nil, domain.ErrValidation
		}
	}
	return query, nil
}
