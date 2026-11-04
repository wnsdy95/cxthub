package http

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strconv"
)

func (s *Server) SetEffectiveMemory(reader inbound.EffectiveMemoryReader) error {
	if err := validateMemoryReader(reader); err != nil {
		return err
	}
	s.effectiveMemory = reader
	return nil
}

func (s *Server) SetMemoryPositions(reader inbound.MemoryPositionReader) error {
	if err := validateMemoryReader(reader); err != nil {
		return err
	}
	s.memoryPositions = reader
	return nil
}

func (s *Server) SetLiveMemoryQueries(reader inbound.LiveMemoryQueries) error {
	if err := validateMemoryReader(reader); err != nil {
		return err
	}
	s.effectiveMemory, s.memoryPositions = reader, reader
	return nil
}
func (s *Server) queryMemoryPositions(w http.ResponseWriter, r *http.Request) {
	if s.memoryPositions == nil {
		s.writeError(w, 503, "unavailable", "Effective memory query unavailable")
		return
	}
	q := r.URL.Query()
	s.memoryReadWithScope(w, r, s.memoryPositions, func(ctx context.Context) (any, error) {
		return s.memoryPositions.QueryMemoryPositions(ctx, s.repoID(r), domain.ContentHash(q.Get("snapshot_id")), q.Get("event_id"))
	})
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
	s.memoryReadWithScope(w, r, s.effectiveMemory, func(ctx context.Context) (any, error) {
		return s.effectiveMemory.QueryEffectiveMemory(ctx, s.repoID(r), domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: q.Get("branch"), SnapshotID: domain.ContentHash(q.Get("snapshot_id")), CodeCommit: q.Get("code_commit"), MemoryHash: domain.ContentHash(q.Get("memory_hash"))}, Content: q.Get("content"), Limit: limit, Cursor: q.Get("cursor")})
	})
}
