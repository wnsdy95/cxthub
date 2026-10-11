package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGitRefObservationLegacyIdentity(t *testing.T) {
	for _, source := range []string{"github-push", "reconciliation"} {
		t.Run(source, func(t *testing.T) {
			legacy := struct {
				Delivery  string      `json:"delivery,omitempty"`
				ID        string      `json:"id"`
				RepoID    ContentHash `json:"repo_id"`
				GitOrigin string      `json:"git_origin"`
				Source    string      `json:"source"`
				Ref       string      `json:"ref"`
				Before    string      `json:"before,omitempty"`
				After     string      `json:"after,omitempty"`
				Forced    bool        `json:"forced"`
			}{Delivery: "delivery", RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: source, Ref: "refs/heads/feature", Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40), Forced: true}
			raw, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			var observation GitRefObservation
			if err := json.Unmarshal(raw, &observation); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(observation)
			if err != nil || string(encoded) != string(raw) {
				t.Fatalf("legacy bytes changed: %s %v", encoded, err)
			}
			expected := gitEvidenceKey(legacy)
			if source == "github-push" {
				expected = gitEvidenceKey([]string{string(legacy.RepoID), legacy.GitOrigin, source, legacy.Delivery})
			}
			observation = observation.WithID()
			if observation.ID != expected || observation.Validate() != nil {
				t.Fatalf("legacy identity changed: %+v", observation)
			}
		})
	}
}

func gitPRObservationFixture() GitRefObservation {
	return GitRefObservation{Delivery: "delivery", RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/acme/api.git", Source: "github-pull-request", Ref: "refs/heads/feature", Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40), PullRequest: &GitPullRequestObservation{InstallationID: 789, RepositoryID: 91, ID: 701, Number: 7, Action: "synchronize", HeadRepositoryID: 91, BaseRepositoryID: 91, BaseRef: "main"}}
}

func TestGitPullRequestObservationValidation(t *testing.T) {
	for _, action := range []string{"synchronize", "opened", "reopened", "closed"} {
		for _, width := range []int{40, 64} {
			observation := gitPRObservationFixture()
			observation.PullRequest.Action = action
			observation.After = strings.Repeat("b", width)
			observation.Before = ""
			if action == "synchronize" {
				observation.Before = strings.Repeat("a", width)
			}
			if action == "closed" {
				observation.PullRequest.MergeSHA = strings.Repeat("c", width)
			}
			if err := observation.WithID().Validate(); err != nil {
				t.Fatalf("%s/%d: %v", action, width, err)
			}
		}
	}
	for name, change := range map[string]func(*GitRefObservation){
		"missing metadata":      func(observation *GitRefObservation) { observation.PullRequest = nil },
		"wrong source":          func(observation *GitRefObservation) { observation.Source = "github-push" },
		"missing delivery":      func(observation *GitRefObservation) { observation.Delivery = "" },
		"long delivery":         func(observation *GitRefObservation) { observation.Delivery = strings.Repeat("a", 129) },
		"missing installation":  func(observation *GitRefObservation) { observation.PullRequest.InstallationID = 0 },
		"missing repo":          func(observation *GitRefObservation) { observation.PullRequest.RepositoryID = 0 },
		"missing PR ID":         func(observation *GitRefObservation) { observation.PullRequest.ID = 0 },
		"missing number":        func(observation *GitRefObservation) { observation.PullRequest.Number = 0 },
		"foreign head":          func(observation *GitRefObservation) { observation.PullRequest.HeadRepositoryID = 92 },
		"foreign base":          func(observation *GitRefObservation) { observation.PullRequest.BaseRepositoryID = 92 },
		"missing base":          func(observation *GitRefObservation) { observation.PullRequest.BaseRef = "" },
		"invalid ref":           func(observation *GitRefObservation) { observation.Ref = "refs/heads/../main" },
		"missing before":        func(observation *GitRefObservation) { observation.Before = "" },
		"short before":          func(observation *GitRefObservation) { observation.Before = "abcdef0" },
		"zero before":           func(observation *GitRefObservation) { observation.Before = strings.Repeat("0", 40) },
		"missing after":         func(observation *GitRefObservation) { observation.After = "" },
		"short after":           func(observation *GitRefObservation) { observation.After = "abcdef0" },
		"zero after":            func(observation *GitRefObservation) { observation.After = strings.Repeat("0", 40) },
		"mixed hash algorithms": func(observation *GitRefObservation) { observation.After = strings.Repeat("b", 64) },
		"unsupported action":    func(observation *GitRefObservation) { observation.PullRequest.Action = "edited" },
		"invented force proof":  func(observation *GitRefObservation) { observation.Forced = true },
		"unexpected merge":      func(observation *GitRefObservation) { observation.PullRequest.MergeSHA = strings.Repeat("c", 40) },
		"opened before":         func(observation *GitRefObservation) { observation.PullRequest.Action = "opened" },
		"closed without merge": func(observation *GitRefObservation) {
			observation.PullRequest.Action, observation.Before = "closed", ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			observation := gitPRObservationFixture()
			change(&observation)
			if err := observation.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("invalid observation accepted: %v", err)
			}
		})
	}
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

func TestGitPRReconciliationObservation(t *testing.T) {
	fixture := func() GitRefObservation {
		observation := gitPRObservationFixture()
		observation.Source = "github-pr-reconciliation"
		observation.PullRequest.Action = "force-pushed"
		observation.PullRequest.ProviderEventID = "provider-event-id"
		observation.PullRequest.OccurredAt = time.Date(2026, 10, 10, 8, 12, 37, 0, time.UTC)
		observation.Forced = true
		return observation
	}
	original := fixture().WithID()
	if err := original.Validate(); err != nil {
		t.Fatal(err)
	}
	changed := fixture()
	changed.After = strings.Repeat("c", 40)
	if changed.WithID().ID != original.ID {
		t.Fatal("altered provider event bypasses delivery conflict")
	}
	closed := fixture()
	closed.Before, closed.Forced, closed.PullRequest.Action = "", false, "closed"
	closed.PullRequest.MergeSHA = strings.Repeat("c", 40)
	if err := closed.WithID().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*GitRefObservation){
		"missing provider event": func(observation *GitRefObservation) { observation.PullRequest.ProviderEventID = "" },
		"oversized provider event": func(observation *GitRefObservation) {
			observation.PullRequest.ProviderEventID = strings.Repeat("a", 257)
		},
		"missing occurrence time": func(observation *GitRefObservation) { observation.PullRequest.OccurredAt = time.Time{} },
		"webhook source":          func(observation *GitRefObservation) { observation.Source = "github-pull-request" },
		"missing delivery":        func(observation *GitRefObservation) { observation.Delivery = "" },
		"not forced":              func(observation *GitRefObservation) { observation.Forced = false },
		"missing before":          func(observation *GitRefObservation) { observation.Before = "" },
		"short before":            func(observation *GitRefObservation) { observation.Before = "abcdef0" },
		"missing after":           func(observation *GitRefObservation) { observation.After = "" },
		"same endpoints":          func(observation *GitRefObservation) { observation.Before = observation.After },
		"invented merge":          func(observation *GitRefObservation) { observation.PullRequest.MergeSHA = strings.Repeat("c", 40) },
		"webhook action": func(observation *GitRefObservation) {
			observation.PullRequest.Action, observation.Forced = "synchronize", false
		},
		"closed forced": func(observation *GitRefObservation) {
			observation.PullRequest.Action, observation.Before = "closed", ""
			observation.PullRequest.MergeSHA = strings.Repeat("c", 40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			observation := fixture()
			change(&observation)
			if err := observation.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("invalid reconciliation observation accepted: %v", err)
			}
		})
	}
}
