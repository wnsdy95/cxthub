package http

import (
	"errors"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) graphReadiness(w http.ResponseWriter, request *http.Request) {
	queries, supported := server.repositoryStates.(inbound.GraphReadinessQuery)
	if !supported {
		server.writeError(w, http.StatusNotImplemented, "graph_readiness_unavailable", "graph readiness query unavailable")
		return
	}
	request, release, ok := server.admitResourceRequest(w, request, app.ResourceInteractive)
	if !ok {
		return
	}
	defer release()
	repo := server.repoID(request)
	authorize := server.docVisibleReadAuthorizer(request, repo)
	if server.repositoryStateProfile == inbound.RepositoryStatePinnedSnapshots {
		authorize = server.memoryReadAuthorizer(request, repo)
	}
	ctx := inbound.WithDocumentReadAuthorizer(request.Context(), authorize)
	result, err := queries.GetGraphReadiness(ctx, repo)
	if errors.Is(err, domain.ErrCatalogUnsupported) {
		server.writeError(w, http.StatusNotImplemented, "graph_readiness_unavailable", "graph readiness requires pinned projection metadata reads")
		return
	}
	server.respond(w, result, err)
}
