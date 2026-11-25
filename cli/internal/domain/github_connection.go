package domain

// What the server says about the connection of a repository with GitHub.
const (
	// GitHubRepositoryConnected: the server reads the repository on GitHub
	// through the GitHub App installation of the repository's owner.
	GitHubRepositoryConnected = "connected"
	// GitHubRepositoryNotConnected: the owner has no installation in use, or
	// it was not told to read this repository.
	GitHubRepositoryNotConnected = "not_connected"
)

// GitHubRepositoryConnection is the server's answer. State is empty for a
// server that does not say, and has a third value for a server without a
// GitHub App; neither is acted on. ConnectURL is the page where an owner of
// the repository connects it.
type GitHubRepositoryConnection struct {
	State      string `json:"state"`
	ConnectURL string `json:"connect_url,omitempty"`
}
