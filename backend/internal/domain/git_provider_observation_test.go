package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func providerObservationFixture(source string) GitRefObservation {
	return GitRefObservation{RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: source, Delivery: "delivery", Ref: "refs/heads/main", After: strings.Repeat("a", 40), Provider: &GitProviderObservation{InstallationID: 789, RepositoryID: 91}}
}

func TestGitProviderObservationVersionedIdentity(t *testing.T) {
	for _, source := range []string{"github-push", "reconciliation"} {
		t.Run(source, func(t *testing.T) {
			qualified := providerObservationFixture(source).WithID()
			legacy := qualified
			legacy.Provider = nil
			legacy = legacy.WithID()
			if qualified.Validate() != nil || len(qualified.ID) != 64 || qualified.ID == legacy.ID || qualified.WithID().ID != qualified.ID {
				t.Fatalf("invalid qualified identity: %+v", qualified)
			}
			raw, err := json.Marshal(qualified)
			if err != nil {
				t.Fatal(err)
			}
			var restored GitRefObservation
			if err := json.Unmarshal(raw, &restored); err != nil || restored.Validate() != nil || *restored.Provider != *qualified.Provider {
				t.Fatalf("qualified roundtrip: %+v %v", restored, err)
			}
			changed := qualified
			changed.Provider = &GitProviderObservation{InstallationID: 790, RepositoryID: 92}
			changed = changed.WithID()
			if (source == "github-push") != (changed.ID == qualified.ID) {
				t.Fatal("delivery identity must reject altered payload; reconciliation must bind provider")
			}
		})
	}
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

func TestGitProviderObservationValidation(t *testing.T) {
	for name, change := range map[string]func(*GitRefObservation){
		"zero installation":   func(observation *GitRefObservation) { observation.Provider.InstallationID = 0 },
		"negative repository": func(observation *GitRefObservation) { observation.Provider.RepositoryID = -1 },
		"unsupported source":  func(observation *GitRefObservation) { observation.Source = "github-pull-request" },
		"mixed oid widths":    func(observation *GitRefObservation) { observation.Before = strings.Repeat("b", 64) },
		"short oid":           func(observation *GitRefObservation) { observation.After = "abc" },
	} {
		t.Run(name, func(t *testing.T) {
			observation := providerObservationFixture("github-push")
			change(&observation)
			if observation.WithID().Validate() == nil {
				t.Fatal("invalid qualified observation accepted")
			}
		})
	}
}
