package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

const (
	// anchoredNoteBodyLimit bounds a request that carries one note.
	anchoredNoteBodyLimit = 64 << 10
	// worktreeReportBodyLimit bounds one machine's worktree list.
	worktreeReportBodyLimit = 1 << 20
)

func (s *Server) anchoredNotes(w http.ResponseWriter) (inbound.AnchoredNotes, bool) {
	service, ok := s.b.(inbound.AnchoredNotes)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "anchored_notes_unavailable", "note service unavailable")
	}
	return service, ok
}

func (s *Server) reportedWorktrees(w http.ResponseWriter) (inbound.ReportedWorktrees, bool) {
	service, ok := s.b.(inbound.ReportedWorktrees)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "worktrees_unavailable", "worktree service unavailable")
	}
	return service, ok
}

// decodeSingleJSON reads one JSON object of at most limit bytes into out and
// refuses unknown fields. It answers false after writing the error.
func (s *Server) decodeSingleJSON(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds transport limit")
			return false
		}
		s.writeDecodeError(w, err)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		s.writeError(w, http.StatusBadRequest, "bad_request", "expected a single JSON object")
		return false
	}
	return true
}

func (s *Server) listAnchoredNotes(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredNotes(w)
	if !ok {
		return
	}
	notes, err := service.ListAnchoredNotes(r.Context(), s.repoID(r))
	s.respond(w, notes, err)
}

// createAnchoredNote stores what the signed-in member wrote about a commit, a
// branch or a worktree.
func (s *Server) createAnchoredNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target *domain.AnchoredNoteTarget `json:"target"`
		Body   *string                    `json:"body"`
	}
	if !s.decodeSingleJSON(w, r, anchoredNoteBodyLimit, &body) {
		return
	}
	if body.Target == nil || body.Body == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "target and body are required")
		return
	}
	service, ok := s.anchoredNotes(w)
	if !ok {
		return
	}
	note, err := service.CreateAnchoredNote(r.Context(), s.repoID(r), *body.Target, *body.Body)
	s.respond(w, note, err)
}

func (s *Server) updateAnchoredNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body *string `json:"body"`
	}
	if !s.decodeSingleJSON(w, r, anchoredNoteBodyLimit, &body) {
		return
	}
	if body.Body == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "body is required")
		return
	}
	service, ok := s.anchoredNotes(w)
	if !ok {
		return
	}
	note, err := service.UpdateAnchoredNote(r.Context(), s.repoID(r), r.PathValue("noteID"), *body.Body)
	s.respond(w, note, err)
}

func (s *Server) deleteAnchoredNote(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredNotes(w)
	if !ok {
		return
	}
	if err := service.DeleteAnchoredNote(r.Context(), s.repoID(r), r.PathValue("noteID")); err != nil {
		s.respond(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putWorktrees takes the worktree list of one of the signed-in member's
// machines and answers that member's worktrees on every machine.
func (s *Server) putWorktrees(w http.ResponseWriter, r *http.Request) {
	var report struct {
		Machine   string                        `json:"machine"`
		Worktrees *[]domain.WorktreeReportEntry `json:"worktrees"`
	}
	if !s.decodeSingleJSON(w, r, worktreeReportBodyLimit, &report) {
		return
	}
	if report.Worktrees == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "worktrees is required")
		return
	}
	service, ok := s.reportedWorktrees(w)
	if !ok {
		return
	}
	worktrees, err := service.ReportWorktrees(r.Context(), s.repoID(r), domain.WorktreeReport{Machine: report.Machine, Worktrees: *report.Worktrees})
	s.respond(w, worktrees, err)
}

func (s *Server) listWorktrees(w http.ResponseWriter, r *http.Request) {
	service, ok := s.reportedWorktrees(w)
	if !ok {
		return
	}
	worktrees, err := service.ListReportedWorktrees(r.Context(), s.repoID(r))
	s.respond(w, worktrees, err)
}

func (s *Server) anchoredSessionCalls(w http.ResponseWriter, r *http.Request) {
	service, ok := s.anchoredSessions(w)
	if !ok {
		return
	}
	calls, err := service.AnchoredSessionCalls(r.Context(), s.repoID(r), domain.ProviderKind(r.PathValue("provider")), r.PathValue("sessionID"))
	s.respond(w, calls, err)
}
