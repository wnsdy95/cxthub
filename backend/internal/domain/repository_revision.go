package domain

// RepositoryRevision is a durable invalidation cursor, scoped to one repo.
// Counters are JSON strings so browser integer precision cannot lose changes.
type RepositoryRevision struct {
	Evidence uint64 `json:"evidence,string,omitempty"`
	Graph    uint64 `json:"graph,string"`
	Pending  uint64 `json:"pending,string"`
}
