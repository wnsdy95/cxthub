package domain

// RepositoryRevision is a durable invalidation cursor, scoped to one repo.
// Counters are JSON strings so browser integer precision cannot lose changes.
type RepositoryRevision struct {
	Evidence uint64 `json:"evidence,string,omitempty"`
	Graph    uint64 `json:"graph,string"`
	Pending  uint64 `json:"pending,string"`
}
type PendingView struct {
	Graph    *GraphState        `json:"graph"`
	Revision RepositoryRevision `json:"revision"`
	Pending  []Pending          `json:"pending"`
	// Snapshots carry raw capture metadata only. Branches is intentionally absent;
	// consumers preserve memberships from their matching graph generation.
	Snapshots []Snapshot `json:"snapshots"`
}
