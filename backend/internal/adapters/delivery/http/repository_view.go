package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"strconv"
)

func (s *Server) repositoryView(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.GetRepositoryView(r.Context(), s.repoID(r))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, struct {
		domain.RepositoryView
		Graph any `json:"graph"`
	}{v, encodeGraphResponse(r, *v.Graph)}, nil)
}

func (s *Server) contextQuery(w http.ResponseWriter, r *http.Request) {
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
	v, err := s.b.QueryContext(r.Context(), s.repoID(r), in)
	s.respond(w, v, err)
}

func (s *Server) graphState(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.QueryGraphState(r.Context(), s.repoID(r), r.URL.Query().Get("position"))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, encodeGraphResponse(r, v), nil)
}

func encodeGraphResponse(r *http.Request, g domain.GraphState) any {
	if r.URL.Query().Get("graph_encoding") == "indexed-v2" {
		return graphwire.EncodeV2(g)
	}
	return graphwire.Encode(g)
}
