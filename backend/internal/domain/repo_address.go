package domain

// A stored repository is identified by the hash of the address it was first
// registered under. A repository registered under an address it is no longer
// reached at, such as a development server's, is given its current address by
// moving it: it gets the identifier of the new address in every place that
// names it.

// RepoAddressMoveRequest is one move as the store carries it out. RepositoryID
// is the workspace the stored repository must belong to.
type RepoAddressMoveRequest struct {
	From         ContentHash
	To           ContentHash
	ToURL        string
	RepositoryID string
	Apply        bool
}

// RepoAddressColumn is a column that names the identifier being moved, and in
// how many rows.
type RepoAddressColumn struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Rows   int64  `json:"rows"`
}

// RepoAddressMove is what a move changed, or what it would change when it was
// not applied.
type RepoAddressMove struct {
	From         ContentHash `json:"from"`
	To           ContentHash `json:"to"`
	FromURL      string      `json:"from_url"`
	ToURL        string      `json:"to_url"`
	RepositoryID string      `json:"repository_id"`
	Applied      bool        `json:"applied"`
	// Changed are the columns whose rows were given the new identifier.
	Changed []RepoAddressColumn `json:"changed"`
	// RemovedJobs counts the finished document jobs that were removed: a
	// job's own identifier is computed from the repository's.
	RemovedJobs int64 `json:"removed_document_jobs"`
	// Kept are columns that record what happened and still name the old
	// identifier. They are left as they are.
	Kept []RepoAddressColumn `json:"kept"`
}
