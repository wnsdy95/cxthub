package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func gitPRObservationFixture() GitRefObservation {
	return GitRefObservation{Delivery: "delivery", RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: "github-pull-request", Ref: "refs/heads/feature", Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40), PullRequest: &GitPullRequestObservation{InstallationID: 789, RepositoryID: 91, ID: 701, Number: 7, Action: "synchronize", HeadRepositoryID: 91, BaseRepositoryID: 91, BaseRef: "main"}}
}

func TestGitPullRequestObservationDeliveryIdentity(t *testing.T) {
	original := gitPRObservationFixture().WithID()
	raw, err := json.Marshal(original)
	if err != nil || strings.Contains(string(raw), "provider_event_id") || strings.Contains(string(raw), "occurred_at") {
		t.Fatalf("absent provenance changed webhook encoding: %s %v", raw, err)
	}
	changed := gitPRObservationFixture()
	changed.PullRequest.Number++
	changed.After = strings.Repeat("c", 40)
	if changed.WithID().ID != original.ID {
		t.Fatal("altered duplicate can evade immutable delivery conflict")
	}
	changed.Delivery = "another-delivery"
	if changed.WithID().ID == original.ID {
		t.Fatal("independent deliveries share an identity")
	}
}
