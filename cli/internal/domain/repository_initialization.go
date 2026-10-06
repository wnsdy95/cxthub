package domain

import "strings"

// RepositoryInitializationReceipt proves server creation, not Git branch birth.
// Repo is the immutable creation metadata; Anchor is an acceptance receipt and
// must never be adopted as the current branch tip on a later retry.
type RepositoryInitializationReceipt struct {
	Version    int                             `json:"version"`
	CreationID string                          `json:"creation_id"`
	Repo       Repo                            `json:"repo"`
	Anchor     *RepositoryInitializationAnchor `json:"anchor,omitempty"`
}

type RepositoryInitializationRequest struct {
	RemoteURL     string `json:"remote_url"`
	GitRemoteURL  string `json:"git_remote_url"`
	DefaultBranch string `json:"default_branch"`
}

type RepositoryInitializationAnchor struct {
	Ref            Ref                         `json:"ref"`
	SnapshotStates map[ContentHash]ContentHash `json:"snapshot_states"`
}

type RepositoryInitializationFinalize struct {
	CreationID string                         `json:"creation_id"`
	Anchor     RepositoryInitializationAnchor `json:"anchor"`
}

func (r RepositoryInitializationReceipt) Validate(repo string) error {
	if ValidateContentHash(repo) != nil || r.Version != 1 || r.Repo.ID != repo || r.Repo.RepositoryID == "" ||
		r.Repo.ContextProtocol != 1 || r.Repo.LocalPath != "" || ValidateBranchName(r.Repo.DefaultBranch) != nil || !validInitializationID(r.CreationID) {
		return ErrHashMismatch
	}
	if r.Anchor != nil {
		return r.Anchor.Validate(repo)
	}
	return nil
}

func (a RepositoryInitializationAnchor) Validate(repo string) error {
	r := a.Ref
	if ValidateContentHash(repo) != nil || ValidateRef(r) != nil || r.Kind != RefBranch || r.Symbolic != "" ||
		r.RepoID != repo || r.BranchID != LegacyContextBranchID(repo, r.Name) || (len(a.SnapshotStates) == 0 || len(a.SnapshotStates) > 10000) {
		return ErrInvalidRef
	}
	if _, ok := a.SnapshotStates[r.Target]; !ok {
		return ErrHashMismatch
	}
	for id, state := range a.SnapshotStates {
		if ValidateContentHash(id) != nil || ValidateContentHash(state) != nil {
			return ErrHashMismatch
		}
	}
	return nil
}

func (r RepositoryInitializationFinalize) Validate(repo string) error {
	if !validInitializationID(r.CreationID) {
		return ErrInvalidRef
	}
	return r.Anchor.Validate(repo)
}

func validInitializationID(id string) bool {
	if len(id) != 37 || !strings.HasPrefix(id, "init_") {
		return false
	}
	for _, ch := range id[5:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
