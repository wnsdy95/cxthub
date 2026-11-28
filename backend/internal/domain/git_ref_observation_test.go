package domain

import (
	"strings"
	"testing"
)

func gitPushObservationFixture() GitRefObservation {
	return GitRefObservation{Delivery: "delivery", RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: "github-push", Ref: "refs/heads/feature", After: strings.Repeat("b", 40), Provider: &GitProviderObservation{InstallationID: 789, RepositoryID: 91}}
}

func TestGitPushObservationDeliveryIdentity(t *testing.T) {
	original := gitPushObservationFixture().WithID()
	// The identity of a qualified push stays what it was before the pull
	// request and unqualified observations were removed.
	if original.ID != gitEvidenceKey([]string{"qualified-git-ref-observation-v1", string(original.RepoID), original.GitOrigin, "github-push", "delivery"}) {
		t.Fatal("the identity of a delivered push changed")
	}
	changed := gitPushObservationFixture()
	changed.Ref = "refs/heads/other"
	changed.After = strings.Repeat("c", 40)
	if changed.WithID().ID != original.ID {
		t.Fatal("altered duplicate can evade immutable delivery conflict")
	}
	changed.Delivery = "another-delivery"
	if changed.WithID().ID == original.ID {
		t.Fatal("independent deliveries share an identity")
	}
}

func TestGitObservationWithoutAQualifiedPushHasNoIdentity(t *testing.T) {
	unqualified := gitPushObservationFixture()
	unqualified.Provider = nil
	other := gitPushObservationFixture()
	other.Source = "reconciliation"
	for name, observation := range map[string]GitRefObservation{"without provider": unqualified, "another source": other} {
		if id := observation.WithID().ID; id != "" {
			t.Fatalf("%s received the identity %s", name, id)
		}
	}
}
