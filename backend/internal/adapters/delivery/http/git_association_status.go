package http

import (
	"net/http"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) getGitAssociationStatus(writer http.ResponseWriter, request *http.Request) {
	query, supported := server.b.(inbound.GitAssociationStatusQuery)
	if !supported {
		server.respond(writer, nil, domain.ErrGitAssociationStatusUnavailable)
		return
	}
	values := request.URL.Query()
	selection := domain.GitAssociationStatusRequest{Kind: "integration", State: "pending", Limit: 30, Cursor: values.Get("cursor")}
	for key, value := range values {
		if len(value) != 1 {
			server.respond(writer, nil, domain.ErrValidation)
			return
		}
		switch key {
		case "kind":
			selection.Kind = value[0]
		case "state":
			selection.State = value[0]
		case "cursor":
		case "limit":
			limit, err := strconv.Atoi(value[0])
			if err != nil {
				server.respond(writer, nil, domain.ErrValidation)
				return
			}
			selection.Limit = limit
		default:
			server.respond(writer, nil, domain.ErrValidation)
			return
		}
	}
	if err := selection.Validate(); err != nil {
		server.respond(writer, nil, err)
		return
	}
	page, err := query.GetGitAssociationStatus(request.Context(), server.repoID(request), selection)
	server.respond(writer, page, err)
}
