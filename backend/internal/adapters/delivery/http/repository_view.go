package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strconv"
)

func (s *Server) repositoryView(w http.ResponseWriter, r *http.Request) {
	s.graphRead(w, r, func(request *http.Request) (any, error) {
		view, err := s.repositoryGraphs.GetRepositoryView(request.Context(), s.repoID(request))
		if err != nil {
			return nil, err
		}
		return struct {
			domain.RepositoryView
			Graph any `json:"graph"`
		}{view, encodeGraphResponse(request, *view.Graph)}, nil
	})
}

func (s *Server) contextQuery(w http.ResponseWriter, r *http.Request) {
	if s.contextQueries == nil {
		s.writeError(w, http.StatusNotImplemented, "context_query_unavailable", "context query unavailable")
		return
	}
	r, release, ok := s.admitResourceRequest(w, r, app.ResourceInteractive)
	if !ok {
		return
	}
	defer release()
	q := r.URL.Query()
	in := domain.ContextSelection{Branch: q.Get("branch"), Position: q.Get("position"), Scope: q.Get("scope"), CodeCommit: q.Get("code_commit"), SegmentStateHash: domain.ContentHash(q.Get("segment_state_hash"))}
	for name, dst := range map[string]*int{"segment_limit": &in.SegmentLimit, "segment_offset": &in.SegmentOffset} {
		if q.Has(name) {
			n, err := strconv.Atoi(q.Get(name))
			if err != nil {
				s.respond(w, nil, domain.ErrValidation)
				return
			}
			*dst = n
		}
	}
	authorize := s.docVisibleReadAuthorizer(r, s.repoID(r))
	if s.contextQueryProfile == inbound.ContextQueryPinnedSnapshots {
		authorize = s.memoryReadAuthorizer(r, s.repoID(r))
	}
	ctx := inbound.WithDocumentReadAuthorizer(r.Context(), authorize)
	v, err := s.contextQueries.QueryContext(ctx, s.repoID(r), in)
	s.respond(w, v, err)
}

func (s *Server) graphState(w http.ResponseWriter, r *http.Request) {
	s.graphRead(w, r, func(request *http.Request) (any, error) {
		view, err := s.repositoryGraphs.QueryGraphState(request.Context(), s.repoID(request), request.URL.Query().Get("position"))
		if err != nil {
			return nil, err
		}
		return encodeGraphResponse(request, view), nil
	})
}

func encodeGraphResponse(r *http.Request, g domain.GraphState) any {
	if r.URL.Query().Get("graph_encoding") == "indexed-v2" {
		return graphwire.EncodeV2(g)
	}
	return graphwire.Encode(g)
}
