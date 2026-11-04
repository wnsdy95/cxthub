package domain

import "context"

func ValidateGraphReadQualification(ctx context.Context, projection GraphProjectionRead) error {
	key := projection.QualificationKey()
	if key.Validate() != nil || projection.ServingFence <= 0 {
		return ErrIntegrity
	}
	result := projection.Qualification
	if result == nil {
		return ErrGraphProjectionNotReady
	}
	if result.Validate() != nil || result.GraphQualificationKey != key {
		return ErrIntegrity
	}
	if !result.Matched() {
		return ErrGraphProjectionFailed
	}
	if len(projection.Parts) != 0 {
		digest, err := GraphViewProjectionManifestDigest(ctx, key.RepoID, projection.Parts)
		if err != nil {
			return err
		}
		if digest != result.Actual {
			return &GraphProjectionCorruption{RepoID: key.RepoID, Dataset: key.Dataset, Applied: key.Applied, Rules: key.Rules,
				Serving: true, ServingFence: projection.ServingFence, Cause: ErrIntegrity}
		}
	}
	return nil
}
