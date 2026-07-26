package domain

// User profile "Contribution activity" feed view model (GitHub response).
// Usable with our data: monthly context commit bundles + workspace creation.
// (PR, review, language, "Built by" omitted due to lack of data.)

// ActivityRepo is a workspace (repo) item in the monthly commit card.
type ActivityRepo struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // owner_username/slug
	Count int    `json:"count"`
}

// ActivityCreated is an item in the monthly "workspace creation" card.
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
