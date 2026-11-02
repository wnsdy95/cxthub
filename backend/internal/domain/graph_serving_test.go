package domain

import "testing"

func TestGraphServingGenerationIncludesPromotionFence(test *testing.T) {
	projection := graphProjectionStatusTestRead()
	candidate, err := projection.Status()
	if err != nil {
		test.Fatal(err)
	}
	projection.ServingFence = 1
	serving, err := projection.Status()
	if err != nil || serving.Generation == candidate.Generation {
		test.Fatal("candidate and serving identities collide", err)
	}
	projection.ServingFence++
	repaired, err := projection.Status()
	if err != nil || repaired.Generation == serving.Generation || repaired.AppliedEpoch != serving.AppliedEpoch {
		test.Fatal("same-candidate repair reused serving identity", err)
	}
	projection.Source.Sequence++
	projection.Ready = false
	lagged, err := projection.Status()
	if err != nil || lagged.Generation != repaired.Generation || lagged.Current || !lagged.Complete {
		test.Fatal("lag changed retained identity", err)
	}
}
