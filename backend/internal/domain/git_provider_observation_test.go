package domain

import (
	"strings"
	"testing"
)

func providerObservationFixture(source string) GitRefObservation {
	return GitRefObservation{RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: source, Delivery: "delivery", Ref: "refs/heads/main", After: strings.Repeat("a", 40), Provider: &GitProviderObservation{InstallationID: 789, RepositoryID: 91}}
}

func TestGitProviderObservationReconciliationDoesNotAssertFreshness(t *testing.T) {
	first := providerObservationFixture("reconciliation").WithID()
	next := first
	next.After = strings.Repeat("b", 40)
	next = next.WithID()
	returned := next
	returned.After = first.After
	returned = returned.WithID()
	if first.ID == next.ID || returned.ID != first.ID {
		t.Fatal("A to B to A must reuse immutable A evidence, not invent a newer observation")
	}
}
