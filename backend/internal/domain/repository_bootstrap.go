package domain

type RepositoryBootstrap struct {
	Revision      RepositoryRevision `json:"revision"`
	DefaultBranch string             `json:"default_branch"`
	Branch        string             `json:"branch"`
	Ref           *Ref               `json:"ref,omitempty"`
	Snapshot      *Snapshot          `json:"snapshot,omitempty"`
	Parent        *Snapshot          `json:"parent,omitempty"`
	Archived      bool               `json:"archived"`
}
