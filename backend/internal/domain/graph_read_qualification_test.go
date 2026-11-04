package domain

import (
	"context"
	"errors"
	"testing"
)

func graphReadQualificationTestProjection(test *testing.T) GraphProjectionRead {
	test.Helper()
	repo := HashContent([]byte(test.Name()))
	parts, err := EncodeGraphViewProjection(context.Background(), repo, graphViewProjectionTestView(repo))
	if err != nil {
		test.Fatal(err)
	}
	digest, err := GraphViewProjectionManifestDigest(context.Background(), repo, parts)
	if err != nil {
		test.Fatal(err)
	}
	applied := RepositoryViewSourceCheckpoint{Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
		RepoID: repo, Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 7}
	source := applied
	source.Sequence++
	projection := GraphProjectionRead{Enabled: true, Ready: true, Complete: true, Rules: GraphViewProjectionRules,
		Dataset: "11111111-1111-4111-8111-111111111111", Source: source, Applied: applied, Parts: parts, ServingFence: 19}
	projection.Qualification = &GraphQualificationResult{GraphQualificationKey: projection.QualificationKey(), Expected: digest, Actual: digest}
	return projection
}

func TestValidateGraphReadQualificationMatched(test *testing.T) {
	for _, name := range []string{"manifest", "nil_parts", "empty_parts"} {
		test.Run(name, func(test *testing.T) {
			projection := graphReadQualificationTestProjection(test)
			switch name {
			case "nil_parts":
				projection.Parts = nil
			case "empty_parts":
				projection.Parts = []GraphProjectionPart{}
			}
			if err := ValidateGraphReadQualification(context.Background(), projection); err != nil {
				test.Fatal("matched qualification rejected", err)
			}
		})
	}
}

func TestValidateGraphReadQualificationRejectsInvalidReceipts(test *testing.T) {
	cases := []struct {
		name   string
		change func(*GraphProjectionRead)
		want   error
	}{
		{
			name: "missing_qualification",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification = nil
			},
			want: ErrGraphProjectionNotReady,
		},
		{
			name: "unmatched_qualification",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.Expected = HashContent([]byte("different view"))
			},
			want: ErrGraphProjectionFailed,
		},
		{
			name: "missing_expected_digest",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.Expected = ""
			},
			want: ErrIntegrity,
		},
		{
			name: "malformed_actual_digest",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.Actual = "invalid"
			},
			want: ErrIntegrity,
		},
		{
			name: "malformed_qualification_key",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.ComparisonRules++
			},
			want: ErrIntegrity,
		},
		{
			name: "wrong_dataset",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.Dataset = "33333333-3333-4333-8333-333333333333"
			},
			want: ErrIntegrity,
		},
		{
			name: "wrong_applied_checkpoint",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.Applied.Sequence++
			},
			want: ErrIntegrity,
		},
		{
			name: "wrong_repository",
			change: func(projection *GraphProjectionRead) {
				projection.Qualification.RepoID = HashContent([]byte("foreign repository"))
				projection.Qualification.Applied.RepoID = projection.Qualification.RepoID
			},
			want: ErrIntegrity,
		},
		{
			name: "invalid_projection_dataset",
			change: func(projection *GraphProjectionRead) {
				projection.Dataset = "invalid"
				projection.Qualification = nil
			},
			want: ErrIntegrity,
		},
		{
			name: "invalid_projection_rules",
			change: func(projection *GraphProjectionRead) {
				projection.Rules++
			},
			want: ErrIntegrity,
		},
		{
			name: "invalid_projection_source_scope",
			change: func(projection *GraphProjectionRead) {
				projection.Applied.Scope = RepositoryViewSourceScope
			},
			want: ErrIntegrity,
		},
		{
			name: "zero_serving_fence",
			change: func(projection *GraphProjectionRead) {
				projection.ServingFence = 0
				projection.Qualification = nil
			},
			want: ErrIntegrity,
		},
		{
			name: "negative_serving_fence",
			change: func(projection *GraphProjectionRead) {
				projection.ServingFence = -1
			},
			want: ErrIntegrity,
		},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			projection := graphReadQualificationTestProjection(test)
			scenario.change(&projection)
			if err := ValidateGraphReadQualification(context.Background(), projection); err != scenario.want {
				test.Fatalf("qualification error = %v, want %v", err, scenario.want)
			}
		})
	}
}

func TestValidateGraphReadQualificationManifestMismatchPreservesServingIdentity(test *testing.T) {
	projection := graphReadQualificationTestProjection(test)
	projection.Qualification.Expected = HashContent([]byte("another matched manifest"))
	projection.Qualification.Actual = projection.Qualification.Expected
	err := ValidateGraphReadQualification(context.Background(), projection)
	var failure *GraphProjectionCorruption
	if !errors.As(err, &failure) || failure == nil {
		test.Fatal("manifest drift did not report corruption", err)
	}
	if failure.RepoID != projection.Applied.RepoID || failure.Dataset != projection.Dataset ||
		failure.Applied != projection.Applied || failure.Rules != projection.Rules ||
		!failure.Serving || failure.ServingFence != projection.ServingFence || failure.Cause != ErrIntegrity ||
		!errors.Is(err, ErrIntegrity) || failure.Validate() != nil {
		test.Fatalf("manifest corruption lost serving identity: %#v", failure)
	}
}

func TestValidateGraphReadQualificationPropagatesManifestErrors(test *testing.T) {
	projection := graphReadQualificationTestProjection(test)
	projection.Parts = projection.Parts[:1]
	if err := ValidateGraphReadQualification(context.Background(), projection); err != ErrIntegrity {
		test.Fatal("manifest hashing error was changed", err)
	}
}

func TestValidateGraphReadQualificationCanceledHashing(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{"manifest", "nil_parts", "empty_parts", "partial_manifest", "missing_qualification", "unmatched_qualification"} {
		test.Run(name, func(test *testing.T) {
			projection := graphReadQualificationTestProjection(test)
			var want error
			switch name {
			case "manifest":
				want = context.Canceled
			case "nil_parts":
				projection.Parts = nil
			case "empty_parts":
				projection.Parts = []GraphProjectionPart{}
			case "partial_manifest":
				projection.Parts = projection.Parts[:1]
				want = ErrIntegrity
			case "missing_qualification":
				projection.Qualification = nil
				want = ErrGraphProjectionNotReady
			case "unmatched_qualification":
				projection.Qualification.Expected = HashContent([]byte("different view"))
				want = ErrGraphProjectionFailed
			}
			if err := ValidateGraphReadQualification(ctx, projection); err != want {
				test.Fatalf("canceled qualification error = %v, want %v", err, want)
			}
		})
	}
}
