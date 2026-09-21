package domain

// RepositoryConnection preserves content identity across product URL changes.
type RepositoryConnection struct {
	RepositoryID  string      `json:"repository_id"`
	RepoID        ContentHash `json:"repo_id"`
	RemoteURL     string      `json:"remote_url"`
	CanonicalPath string      `json:"canonical_path"`
}
