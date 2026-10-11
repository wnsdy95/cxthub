package domain

type RepositoryMetadataGeneration struct {
	RepoID   ContentHash
	Epoch    string
	Sequence int64
}

func (generation RepositoryMetadataGeneration) Validate() error {
	if ValidateContentHash(generation.RepoID) != nil || !catalogEpochValid(generation.Epoch) || generation.Sequence < 0 {
		return ErrValidation
	}
	return nil
}
