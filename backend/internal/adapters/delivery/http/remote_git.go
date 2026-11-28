package http

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) remoteGit(w http.ResponseWriter) (inbound.RemoteGit, bool) {
	service, ok := s.b.(inbound.RemoteGit)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "remote_git_unavailable", "remote Git service unavailable")
	}
	return service, ok
}

// remoteGitCommitView is a commit as a member reads it. The author's email is
// not part of it.
type remoteGitCommitView struct {
	SHA         string    `json:"sha"`
	Parents     []string  `json:"parents"`
	AuthorName  string    `json:"author_name"`
	AuthoredAt  time.Time `json:"authored_at"`
	CommittedAt time.Time `json:"committed_at"`
	Subject     string    `json:"subject"`
}

// remoteGitCommitsView is a page of the commit list. Next is where the
// following page starts, absent on the last page.
type remoteGitCommitsView struct {
	Commits []remoteGitCommitView `json:"commits"`
	Next    string                `json:"next,omitempty"`
}

// remoteGitView is the page from the top of the commit list, which also
// carries the default branch, the branches and the pull requests.
type remoteGitView struct {
	DefaultBranch string                `json:"default_branch,omitempty"`
	SyncedAt      *time.Time            `json:"synced_at,omitempty"`
	Branches      []domain.RemoteBranch `json:"branches"`
	Pulls         []domain.RemotePull   `json:"pulls"`
	remoteGitCommitsView
}

// getRemoteGit reads what the server keeps of the repository's commits,
// branches and pull requests. With before, the answer is the page of commits
// that follows and nothing else.
func (s *Server) getRemoteGit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := domain.RemoteGitPageDefault
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
		limit = parsed
	}
	before, err := domain.ParseRemoteCommitCursor(query.Get("before"))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	service, ok := s.remoteGit(w)
	if !ok {
		return
	}
	page, err := service.ReadRemoteGit(r.Context(), s.repoID(r), before, limit)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	commits := remoteGitCommitsView{Commits: make([]remoteGitCommitView, 0, len(page.Commits))}
	for _, commit := range page.Commits {
		commits.Commits = append(commits.Commits, remoteGitCommitView{SHA: commit.SHA, Parents: append([]string{}, commit.Parents...),
			AuthorName: commit.AuthorName, AuthoredAt: commit.AuthoredAt, CommittedAt: commit.CommittedAt, Subject: commit.Subject})
	}
	if page.Next != nil {
		commits.Next = page.Next.String()
	}
	if !before.IsZero() {
		s.respond(w, commits, nil)
		return
	}
	view := remoteGitView{DefaultBranch: page.DefaultBranch, SyncedAt: page.SyncedAt, Branches: append([]domain.RemoteBranch{}, page.Branches...),
		Pulls: make([]domain.RemotePull, 0, len(page.Pulls)), remoteGitCommitsView: commits}
	for _, pull := range page.Pulls {
		pull.Commits = append([]string{}, pull.Commits...)
		view.Pulls = append(view.Pulls, pull)
	}
	s.respond(w, view, nil)
}

// refreshRemoteGit asks for the provider to be read again soon. The answer
// says that the request was taken, not that anything was read.
func (s *Server) refreshRemoteGit(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return
	}
	service, ok := s.remoteGit(w)
	if !ok {
		return
	}
	if err := service.RequestRemoteGitRefresh(r.Context(), s.repoID(r)); err != nil {
		s.respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Requested bool `json:"requested"`
	}{true})
}
