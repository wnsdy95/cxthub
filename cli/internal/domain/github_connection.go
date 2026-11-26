package domain

// What the server says about the connection of a repository with GitHub.
const (
	// GitHubRepositoryConnected: the server reads the repository on GitHub
	// through a GitHub App installation that was told to read it.
	GitHubRepositoryConnected = "connected"
	// GitHubRepositoryNotConnected: no installation in use was told to read
	// this repository.
	GitHubRepositoryNotConnected = "not_connected"
)

// GitHubRepositoryConnection is the server's answer. State is empty for a
// server that does not say, and has a third value for a server without a
// GitHub App; neither is acted on. ConnectURL is the page where a maintainer
// of the repository connects it.
type GitHubRepositoryConnection struct {
	State      string `json:"state"`
	ConnectURL string `json:"connect_url,omitempty"`
}
