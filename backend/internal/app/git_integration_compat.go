package app

func (service *Service) gitIntegration() *GitIntegrationWorkers {
	revisions, err := service.RepositoryStateQueries()
	return &GitIntegrationWorkers{work: service.repositoryWork(), evidence: service.graphEvidenceReader(), memory: service.memoryProjectionReader(),
		requireCompleteMemory: service.requireCompleteMemory, revisions: revisions, revisionError: err}
}

func (scans *GitScans) integration() *GitIntegrationWorkers {
	workers := scans.core.gitIntegration()
	workers.store, workers.reader, workers.sourceAccess = scans.store, scans.reader, scans.sourceAccess
	return workers
}
