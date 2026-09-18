package domain

// RepositoryView is one committed generation of graph metadata. Document bodies
// remain independently addressable by immutable content hash.
type RepositoryView struct {
	Semantics ContextSemantics   `json:"semantics"`
	Revision  RepositoryRevision `json:"revision"`
	Refs      []Ref              `json:"refs"`
	Snapshots []Snapshot         `json:"snapshots"`
	Reflog    []RefLogEntry      `json:"reflog"`
	History   []HistoryEvent     `json:"history"`
	Pending   []Pending          `json:"pending"`
	Unsync    []Unsync           `json:"unsync"`
}
