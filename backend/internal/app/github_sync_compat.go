package app

import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"

func (connections *GitHubConnections) GitHubWorkerConfig(profile GitHubWorkerProfile) GitHubWorkerConfig {
	config := GitHubWorkerConfig{Profile: profile, Store: connections.store, Remote: connections.remote}
	if connections.id != nil {
		config.Repositories = connections.id.repositories
		config.Organizations = connections.id.organization
		config.Teams = connections.id.teams
		config.IdentityTransactions, _ = connections.id.repositories.(outbound.IdentityTransactions)
	}
	if connections.core != nil {
		config.Metadata = connections.core.meta
		config.Blobs = connections.core.blobs
	}
	return config
}

func (connections *GitHubConnections) worker() *GitHubWorker {
	config := connections.GitHubWorkerConfig(GitHubWorkerCompatibility)
	return assembleGitHubWorker(config, connections.core.repositoryWork())
}
