package domain

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
)

var ErrRepositoryInitializationUnsupported = errors.New("repository initialization requires transactional storage")
var ErrRepositoryInitializationConflict = errors.New("repository initialization conflicts with existing state")

const RepositoryInitializationVersion = 1

// Creation metadata is immutable. It does not describe the current repository
// after finalization; use GetRepo for that projection.
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
type RepositoryInitializationReceipt struct {
	Version    int                             `json:"version"`
	CreationID string                          `json:"creation_id"`
	Repo       Repo                            `json:"repo"`
	Anchor     *RepositoryInitializationAnchor `json:"anchor,omitempty"`
}

func ValidateRepositoryInitializationID(id string) error {
	raw := strings.TrimPrefix(id, "init_")
	if len(raw) != 32 || id != "init_"+strings.ToLower(raw) {
		return ErrValidation
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return ErrValidation
	}
	return nil
}
func (a RepositoryInitializationAnchor) Validate(repo ContentHash) error {
	r := a.Ref
	if ValidateContentHash(repo) != nil || ValidateRef(r) != nil || r.RepoID != repo ||
		r.Kind != RefBranch || r.Symbolic != "" || r.BranchID != LegacyContextBranchID(string(repo), r.Name) {
		return ErrValidation
	}
	if len(a.SnapshotStates) == 0 || len(a.SnapshotStates) > 10000 {
		return ErrValidation
	}
	if _, ok := a.SnapshotStates[r.Target]; !ok {
		return ErrValidation
	}
	for id, state := range a.SnapshotStates {
		if ValidateContentHash(id) != nil || ValidateContentHash(state) != nil {
			return ErrValidation
		}
	}
	return nil
}
func (r RepositoryInitializationReceipt) Validate() error {
	if r.Version != RepositoryInitializationVersion || ValidateRepositoryInitializationID(r.CreationID) != nil ||
		ValidateContentHash(r.Repo.ID) != nil || ValidateRepositoryID(r.Repo.RepositoryID) != nil ||
		ValidateBranchName(r.Repo.DefaultBranch) != nil || r.Repo.ContextProtocol != 1 || r.Repo.LocalPath != "" ||
		r.Repo.RemoteURL == "" {
		return ErrIntegrity
	}
	if r.Anchor != nil {
		return r.Anchor.Validate(r.Repo.ID)
	}
	return nil
}

// Equal is payload equality, never ID-only acceptance. Map order is irrelevant.
func (a RepositoryInitializationAnchor) Equal(b RepositoryInitializationAnchor) bool {
	return reflect.DeepEqual(a, b)
}

// ValidateRepositoryInitializationBranch is shared by coherent discovery and
// final apply. Ordinary observations may precede a legacy ref; lifecycle or
// competing identity evidence cannot be erased by observing an old Git name.
func ValidateRepositoryInitializationBranch(repo ContentHash, branch string, refs []Ref, events []HistoryEvent, log []RefLogEntry) error {
	if ValidateContentHash(repo) != nil || ValidateBranchName(branch) != nil {
		return ErrValidation
	}
	legacy := LegacyContextBranchID(string(repo), branch)
	for _, ref := range refs {
		if ref.Kind == RefBranch && ref.Name == branch {
			return ErrRepositoryInitializationConflict
		}
		lifecycle, ok, err := ParseBranchLifecycleRef(ref)
		if err != nil {
			return err
		}
		if ok && lifecycle.Branch == branch {
			return ErrRepositoryInitializationConflict
		}
	}
	for _, entry := range log {
		if entry.Kind == RefBranch && entry.Name == branch {
			return ErrRepositoryInitializationConflict
		}
	}
	for _, e := range events {
		relevant := e.Branch == branch || e.PreviousBranch == branch || e.BranchID == legacy
		if relevant {
			if e.Kind != "position" && e.Kind != "publish" {
				return ErrRepositoryInitializationConflict
			}
			if e.BranchID != legacy {
				return ErrRepositoryInitializationConflict
			}
		}
		// A foreign birth can depend on this still-unpublished legacy branch. It
		// cannot assert that the same name already belonged to a different identity.
		if c := e.Creation; c != nil && c.OriginBranch == branch && c.OriginBranchID != legacy {
			return ErrRepositoryInitializationConflict
		}
		if p := e.PR; p != nil && ((p.HeadBranch == branch && e.SourceBranchID != legacy) || (p.BaseBranch == branch && e.BranchID != legacy)) {
			return ErrRepositoryInitializationConflict
		}
	}
	return nil
}

// Mutable defaults/protection and a previously empty origin may change after
// creation. Repository binding and an already established origin may not.
func ValidateRepositoryInitializationContinuity(created, current Repo) error {
	if created.ID != current.ID || created.RepositoryID != current.RepositoryID ||
		created.RemoteURL != current.RemoteURL || current.ContextProtocol != 1 ||
		(created.GitRemoteURL != "" && created.GitRemoteURL != current.GitRemoteURL) {
		return ErrRepositoryInitializationConflict
	}
	return nil
}
