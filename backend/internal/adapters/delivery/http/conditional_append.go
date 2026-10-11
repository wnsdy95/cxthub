package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) conditionalAppendV1(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BranchID         string             `json:"branch_id"`
		Target           domain.ContentHash `json:"target"`
		ExpectedTarget   domain.ContentHash `json:"expected_target"`
		ExpectedRevision *struct {
			Graph    *uint64 `json:"graph,string"`
			Evidence *uint64 `json:"evidence,string"`
			Pending  uint64  `json:"pending,string"`
		} `json:"expected_revision"`
		Force  bool  `json:"force"`
		Append *bool `json:"append"`
	}
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&body)
	if decodeErr == nil {
		var trailing any
		decodeErr = decoder.Decode(&trailing)
		if decodeErr == io.EOF {
			decodeErr = nil
		} else if decodeErr == nil {
			decodeErr = fmt.Errorf("exactly one request object required")
		}
	}
	if decodeErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(decodeErr, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
		} else {
			s.writeDecodeError(w, decodeErr)
		}
		return
	}
	if body.BranchID == "" || body.ExpectedRevision == nil || body.ExpectedRevision.Graph == nil || body.ExpectedRevision.Evidence == nil || body.Force || (body.Append != nil && !*body.Append) {
		s.writeError(w, http.StatusBadRequest, "bad_request", "conditional append requires branch identity, target and graph/evidence revision without force")
		return
	}
	if err := domain.ValidateContentHash(body.ExpectedTarget); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ref := domain.Ref{RepoID: s.repoID(r), Kind: domain.RefBranch, Name: r.PathValue("name"), BranchID: body.BranchID, Target: body.Target}
	if err := domain.ValidateRef(ref); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	revision := domain.RepositoryRevision{Graph: *body.ExpectedRevision.Graph, Evidence: *body.ExpectedRevision.Evidence, Pending: body.ExpectedRevision.Pending}
	out, err := s.b.UpdateRef(r.Context(), inbound.UpdateRefInput{RepoID: ref.RepoID, Ref: ref, Append: true, ExpectedTarget: body.ExpectedTarget, ExpectedRevision: &revision})
	s.respond(w, out, err)
}
