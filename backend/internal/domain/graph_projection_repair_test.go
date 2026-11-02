package domain

import (
	"context"
	"errors"
	"testing"
)

func TestGraphProjectionCorruptionIdentifiesOnlyCompletedOutput(test *testing.T) {
	repo := HashContent([]byte("graph repair"))
	valid := GraphProjectionCorruption{RepoID: repo, Dataset: "22222222-2222-4222-8222-222222222222",
		Applied: RepositoryViewSourceCheckpoint{Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 1},
		Rules: GraphViewProjectionRules, Cause: ErrIntegrity}
	if err := valid.Validate(); err != nil || !errors.Is(&valid, ErrIntegrity) {
		test.Fatal("valid completed output failure", err)
	}
	for _, change := range []func(*GraphProjectionCorruption){
		func(failure *GraphProjectionCorruption) { failure.RepoID = HashContent([]byte("foreign")) },
		func(failure *GraphProjectionCorruption) { failure.Dataset = "invalid" },
		func(failure *GraphProjectionCorruption) { failure.Applied.Sequence = -1 },
		func(failure *GraphProjectionCorruption) { failure.Applied.Scope = RepositoryViewSourceScope },
		func(failure *GraphProjectionCorruption) { failure.Rules = GraphProjectionRules },
		func(failure *GraphProjectionCorruption) { failure.Serving = true },
		func(failure *GraphProjectionCorruption) { failure.ServingFence = 1 },
		func(failure *GraphProjectionCorruption) { failure.Cause = context.Canceled },
		func(failure *GraphProjectionCorruption) { failure.Cause = errors.Join(ErrIntegrity, context.Canceled) },
		func(failure *GraphProjectionCorruption) {
			failure.Cause = errors.Join(ErrValidation, context.DeadlineExceeded)
		},
		func(failure *GraphProjectionCorruption) { failure.Cause = errors.New("connection failed") },
		func(failure *GraphProjectionCorruption) { failure.Cause = nil },
	} {
		failure := valid
		change(&failure)
		if !errors.Is(failure.Validate(), ErrValidation) {
			test.Fatalf("invalid failure accepted: %+v", failure)
		}
	}
	valid.Serving, valid.ServingFence = true, 1
	if err := valid.Validate(); err != nil {
		test.Fatal("valid serving receipt rejected", err)
	}
	valid.ServingFence = -1
	if !errors.Is(valid.Validate(), ErrValidation) {
		test.Fatal("negative serving fence accepted")
	}
}
