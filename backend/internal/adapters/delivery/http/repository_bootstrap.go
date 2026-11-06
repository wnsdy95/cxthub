package http

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) repositoryBootstrap(w http.ResponseWriter, request *http.Request) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query["branch"]) > 1 {
		server.respond(w, nil, domain.ErrValidation)
		return
	}
	queries, supported := server.repositoryStates.(inbound.RepositoryBootstrapQuery)
	if !supported {
		server.writeError(w, http.StatusNotImplemented, "repository_bootstrap_unavailable", "repository bootstrap unavailable")
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
	view, err := queries.GetRepositoryBootstrap(ctx, repo, query.Get("branch"))
	if errors.Is(err, domain.ErrCatalogUnsupported) {
		server.writeError(w, http.StatusNotImplemented, "repository_bootstrap_unavailable", "repository bootstrap requires pinned reads and context protocol 1")
		return
	}
	server.respond(w, view, err)
}
