package domain

const RepositoryMemorySourceVersion = 1

type RepositoryMemorySourceGeneration struct {
	Version  int                            `json:"version"`
	Graph    RepositoryViewSourceCheckpoint `json:"graph"`
	Epoch    string                         `json:"epoch"`
	Sequence int64                          `json:"sequence,string"`
}

func (generation RepositoryMemorySourceGeneration) Validate() error {
	if generation.Version != RepositoryMemorySourceVersion {
		return ErrCatalogResetRequired
	}
	if generation.Graph.Validate() != nil || generation.Graph.Scope != RepositoryGraphSourceScope ||
		!catalogEpochValid(generation.Epoch) || generation.Epoch == "00000000-0000-0000-0000-000000000000" || generation.Sequence < 0 {
		return ErrValidation
	}
	return nil
}
