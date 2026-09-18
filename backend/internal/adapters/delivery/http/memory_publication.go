package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
)

func (s *Server) publishMemoryArchive(w http.ResponseWriter, r *http.Request) {
	publisher, ok := s.b.(inbound.MemoryPublisher)
	if !ok {
		s.writeError(w, 503, "unavailable", "Memory archive publication unavailable")
		return
	}
	var body struct {
		Objects objectsBody         `json:"objects"`
		Memory  domain.MemoryDigest `json:"memory"`
	}
	if !s.decodeLimited(w, r, &body, 32<<20) {
		return
	}
	o := body.Objects
	hash, err := publisher.PublishMemoryArchive(r.Context(), inbound.MemoryPublication{
		Objects: inbound.CommitInput{RepoID: s.repoID(r), Snapshots: o.Snapshots, Docs: o.Docs, ChunkedDocs: o.ChunkedDocs, ChunkObjects: o.ChunkObjects},
		Memory:  body.Memory,
	})
	s.respond(w, map[string]any{"memory_hash": hash}, err)
}
