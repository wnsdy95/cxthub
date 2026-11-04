package domain

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestGraphQualificationIdentityAndDigests(test *testing.T) {
	repo := HashContent([]byte(test.Name()))
	key := GraphQualificationKey{RepoID: repo, Dataset: "11111111-1111-4111-8111-111111111111", Rules: GraphViewProjectionRules,
		ComparisonRules: GraphQualificationRules, Applied: RepositoryViewSourceCheckpoint{Version: RepositoryViewSourceVersion,
			Scope: RepositoryGraphSourceScope, RepoID: repo, Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 7}}
	if err := key.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*GraphQualificationKey){
		func(key *GraphQualificationKey) { key.RepoID = HashContent([]byte("foreign")) },
		func(key *GraphQualificationKey) { key.Dataset = "invalid" },
		func(key *GraphQualificationKey) { key.Rules++ },
		func(key *GraphQualificationKey) { key.ComparisonRules++ },
		func(key *GraphQualificationKey) { key.Applied.Scope = RepositoryViewSourceScope },
		func(key *GraphQualificationKey) { key.Applied.Sequence = -1 },
	} {
		invalid := key
		mutate(&invalid)
		if invalid.Validate() == nil {
			test.Fatal("invalid qualification key accepted", invalid)
		}
	}
	lease := GraphQualificationLease{GraphQualificationKey: key, Fence: 1, LeaseUntil: time.Now().Add(time.Minute)}
	if lease.Validate() != nil {
		test.Fatal("valid lease rejected")
	}
	for _, expected := range []ContentHash{"", HashContent([]byte("captured reference")), "bad-hash"} {
		lease.ReferenceExpected = expected
		if (lease.Validate() == nil) != (expected != "bad-hash") {
			test.Fatal("invalid reference lease validation", expected)
		}
		reference := GraphProjectionReference{GraphQualificationKey: key, Expected: expected}
		if (reference.Validate() == nil) != (expected != "" && expected != "bad-hash") {
			test.Fatal("invalid captured reference validation", expected)
		}
	}
	lease.ReferenceExpected = ""
	lease.Fence = 0
	if lease.Validate() == nil {
		test.Fatal("unfenced lease accepted")
	}
	view := graphViewProjectionTestView(repo)
	digest, err := GraphViewDigest(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	result := GraphQualificationResult{GraphQualificationKey: key, Expected: digest, Actual: digest}
	if !result.Matched() {
		test.Fatal("matching complete views rejected")
	}
	view.Graph.Operations.Merges[0].identity += "changed"
	result.Actual, err = GraphViewDigest(context.Background(), repo, view)
	if err != nil || result.Matched() || result.Validate() != nil {
		test.Fatal("private merge identity lost in comparison", err)
	}
	result.Actual = ""
	if result.Validate() == nil || result.Matched() {
		test.Fatal("missing digest qualified")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GraphViewDigest(ctx, repo, view); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled digest succeeded", err)
	}
}

func TestGraphQualificationManifestCanonicalization(test *testing.T) {
	repo := HashContent([]byte(test.Name()))
	view := graphViewProjectionTestView(repo)
	want, err := GraphViewDigest(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	parts, err := EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	for index := range parts {
		var value map[string]any
		if err := json.Unmarshal(parts[index].Value, &value); err != nil {
			test.Fatal(err)
		}
		if parts[index].Kind == "header" {
			slices.Reverse(value["parts"].([]any))
		}
		parts[index].Value, err = json.MarshalIndent(value, "", " ")
		if err != nil {
			test.Fatal(err)
		}
	}
	slices.Reverse(parts)
	if got, err := GraphViewProjectionManifestDigest(context.Background(), repo, parts); err != nil || got != want {
		test.Fatal("manifest key/field order or JSONB layout changed proof", got, want, err)
	}
	if _, err := GraphViewProjectionManifestDigest(context.Background(), HashContent([]byte("foreign")), parts); err == nil {
		test.Fatal("foreign proof accepted")
	}
	if _, err := GraphViewProjectionManifestDigest(context.Background(), repo, parts[:1]); err == nil {
		test.Fatal("partial proof accepted")
	}
}
