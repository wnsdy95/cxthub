package domain

import "time"

// User profile "Contribution activity" feed view model (GitHub response).
// Counts are repository-wide finalized publications, not a user's authored Git
// commits. History contains neither Git authors nor Git author/committer dates.
// One Git revision per content repository counts once, on the earliest retained
// publication's CreatedAt UTC date (recorded event time, not server receipt time).
// Snapshot metadata and unfinalized observations are not contribution evidence.
// (PR, review, language, "Built by" omitted due to lack of data.)

func CommitActivityDays(repo ContentHash, history []HistoryEvent) (map[string]int, error) {
	firstPublication := make(map[string]time.Time)
	for _, event := range history {
		if event.Kind != "publish" {
			continue
		}
		if event.RepoID != string(repo) || ValidateHistoryEvent(event) != nil {
			return nil, ErrIntegrity
		}
		if prior, exists := firstPublication[event.GitAfter]; !exists || event.CreatedAt.Before(prior) {
			firstPublication[event.GitAfter] = event.CreatedAt
		}
	}
	days := make(map[string]int)
	for _, recordedAt := range firstPublication {
		days[recordedAt.UTC().Format("2006-01-02")]++
	}
	return days, nil
}

// ActivityRepo is a repository (repo) item in the monthly commit card.
type ActivityRepo struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // owner_username/slug
	Count int    `json:"count"`
}

// ActivityCreated is an item in the monthly "repository creation" card.
type ActivityCreated struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Visibility string `json:"visibility"` // public | private
	Date       string `json:"date"`       // YYYY-MM-DD
}

// ActivityMonth is a bundle of activities for a month (newest month first).
type ActivityMonth struct {
	Month       string            `json:"month"` // "2006-01"
	CommitTotal int               `json:"commit_total"`
	CommitRepos []ActivityRepo    `json:"commit_repos"`
	Created     []ActivityCreated `json:"created"`
}
