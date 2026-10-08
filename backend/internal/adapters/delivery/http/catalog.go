package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) pullCatalog(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	var request domain.CatalogRequest
	var after domain.CatalogCheckpoint
	seen, err := decodeCatalogFields(d, map[string]any{
		"version": &request.Version, "after": &after, "cursor": &request.Cursor, "limit": &request.Limit,
	}, "version")
	if seen["after"] {
		request.After = &after
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
			s.writeError(w, http.StatusBadRequest, "bad_request", "one valid catalog request required")
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
	query, ok := s.b.(inbound.CatalogQuery)
	if !ok {
		s.writeCatalogError(w, domain.ErrCatalogUnsupported)
		return
	}
	out, err := query.CatalogChanges(r.Context(), s.repoID(r), request)
	if err != nil {
		s.writeCatalogError(w, err)
		return
	}
	if out.Entries == nil {
		out.Entries = []domain.CatalogEntry{}
	}
	s.respond(w, out, nil)
}

// Match the wire schema exactly: encoding/json otherwise accepts duplicate or
// case-insensitive keys and silently treats null integers as omitted values.
// Both objects are fixed-depth; cursor contents remain adapter-owned.
func decodeCatalogFields(d *json.Decoder, fields map[string]any, required ...string) (map[string]bool, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, domain.ErrValidation
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, domain.ErrValidation
		}
		value, ok := fields[key]
		if !ok || seen[key] {
			return nil, domain.ErrValidation
		}
		seen[key] = true
		if checkpoint, ok := value.(*domain.CatalogCheckpoint); ok {
			if _, err := decodeCatalogFields(d, map[string]any{
				"version": &checkpoint.Version, "repo_id": &checkpoint.RepoID,
				"epoch": &checkpoint.Epoch, "sequence": &checkpoint.Sequence,
			}, "version", "repo_id", "epoch", "sequence"); err != nil {
				return nil, err
			}
			continue
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if bytes.Equal(raw, []byte("null")) {
			return nil, domain.ErrValidation
		}
		if err := json.Unmarshal(raw, value); err != nil {
			return nil, err
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	for _, key := range required {
		if !seen[key] {
			return nil, domain.ErrValidation
		}
	}
	return seen, nil
}

func (s *Server) writeCatalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCatalogResetRequired):
		s.writeError(w, http.StatusConflict, "reset_required", err.Error())
	case errors.Is(err, domain.ErrCatalogUnsupported):
		s.writeError(w, http.StatusNotImplemented, "catalog_unsupported", err.Error())
	case errors.Is(err, domain.ErrValidation):
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.respond(w, nil, err)
	}
}
