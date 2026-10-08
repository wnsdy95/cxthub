package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
)

// A runtime capability, never persisted as repository configuration. Old
// clients ignore it; new clients treat an absent/zero version as unsupported.
type repoPullView struct {
	InitialAnchorAvailable bool `json:"initial_anchor_available,omitempty"`
	domain.Repo
	BranchPullVersion    int `json:"branch_pull_version,omitempty"`
	CatalogMerkleVersion int `json:"catalog_merkle_version,omitempty"`
	CatalogVersion       int `json:"catalog_version,omitempty"`
}

func (s *Server) pullBranchPlan(w http.ResponseWriter, r *http.Request) {
	var in domain.BranchPullRequest
	if !s.decodeLimited(w, r, &in, 64<<10) {
		return
	}
	if err := in.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	out, err := s.b.PullBranchPlan(r.Context(), s.repoID(r), in)
	s.respond(w, out, err)
}
