package domain

type DocReadGenerationBinding struct {
	SourceID        string
	SourceVersion   int64
	RangeGeneration ContentHash
}

func (binding DocReadGenerationBinding) Validate() error {
	if !catalogEpochValid(binding.SourceID) || binding.SourceID == "00000000-0000-0000-0000-000000000000" ||
		binding.SourceVersion < 0 || ValidateContentHash(binding.RangeGeneration) != nil {
		return ErrIntegrity
	}
	return nil
}
