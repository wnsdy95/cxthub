package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) pullCatalogMerkle(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	var request domain.CatalogMerkleRequest
	var checkpoint domain.CatalogCheckpoint
	seen, err := decodeCatalogFields(d, map[string]any{
		"version": &request.Version, "root_hash": &request.RootHash, "checkpoint": &checkpoint,
		"prefix": &request.Prefix, "offset": &request.Offset, "limit": &request.Limit,
	}, "version")
	if seen["checkpoint"] {
		request.Checkpoint = &checkpoint
	}
	if err == nil {
		if trailing := d.Decode(new(any)); trailing != io.EOF {
			err = trailing
			if err == nil {
				err = domain.ErrValidation
			}
		}
	}
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "catalog request exceeds 64 KiB")
		} else {
			s.writeError(w, http.StatusBadRequest, "bad_request", "one valid Merkle request required")
		}
		return
	}
	if domain.ValidateContentHash(s.repoID(r)) != nil {
		s.writeCatalogError(w, domain.ErrValidation)
		return
	}
	if err := request.Validate(); err != nil {
		s.writeCatalogError(w, err)
		return
	}
	query, ok := s.b.(inbound.CatalogMerkleQuery)
	if !ok {
		s.writeCatalogError(w, domain.ErrCatalogUnsupported)
		return
	}
	out, err := query.CatalogMerkle(r.Context(), s.repoID(r), request)
	if err != nil {
		s.writeCatalogError(w, err)
		return
	}
	s.respond(w, out, nil)
}
