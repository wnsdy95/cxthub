package domain

// RepositoryRevision is a durable invalidation cursor, scoped to one repo.
// Counters are JSON strings so browser integer precision cannot lose changes.
type RepositoryRevision struct {
	Graph   uint64 `json:"graph,string"`
	Pending uint64 `json:"pending,string"`
}
type PendingView struct {
	Revision  RepositoryRevision `json:"revision"`
	Pending   []Pending          `json:"pending"`
	Snapshots []Snapshot         `json:"snapshots"`
}
