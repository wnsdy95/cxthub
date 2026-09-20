package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strconv"
)

func (s *Server) SetEffectiveMemory(q inbound.EffectiveMemoryQuery) { s.effectiveMemory = q }
func (s *Server) SetMemoryPositions(q inbound.MemoryPositionQuery)  { s.memoryPositions = q }
func (s *Server) queryMemoryPositions(w http.ResponseWriter, r *http.Request) {
	if s.memoryPositions == nil {
		s.writeError(w, 503, "unavailable", "Effective memory query unavailable")
		return
	}
	q := r.URL.Query()
	out, err := s.memoryPositions.QueryMemoryPositions(r.Context(), s.repoID(r), domain.ContentHash(q.Get("snapshot_id")), q.Get("event_id"))
	s.respond(w, out, err)
}

func (s *Server) queryEffectiveMemory(w http.ResponseWriter, r *http.Request) {
	if s.effectiveMemory == nil {
		s.writeError(w, 503, "unavailable", "Effective memory query unavailable")
		return
	}
	q := r.URL.Query()
	limit := 0
	if q.Has("limit") {
		var err error
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 50 {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
	}
	out, err := s.effectiveMemory.QueryEffectiveMemory(r.Context(), s.repoID(r), domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: domain.ContentHash(q.Get("snapshot_id")), CodeCommit: q.Get("code_commit"), MemoryHash: domain.ContentHash(q.Get("memory_hash"))}, Content: q.Get("content"), Limit: limit, Cursor: q.Get("cursor")})
	s.respond(w, out, err)
}
