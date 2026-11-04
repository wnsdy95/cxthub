package app

func (service *Service) PRPromotionWorkerConfig(profile PRPromotionProfile) PRPromotionWorkerConfig {
	return PRPromotionWorkerConfig{Profile: profile, Metadata: service.meta, Blobs: service.blobs,
		Engine: service.engine, Repositories: service.repositories, Evidence: service.graphEvidenceReader(),
		RequireCompleteMemory: service.requireCompleteMemory, CapturedEvidence: service.projectionEvidence != nil}
}

func (service *Service) prPromotion() *PRPromotionWorker {
	return assemblePRPromotionWorker(service.PRPromotionWorkerConfig(PRPromotionCompatibility), service.repositoryWork())
}

func (service *Service) repositoryRefs() *repositoryRefCommands {
	return repositoryRefCommandsForWork(service.meta, service.engine, service.repositories, service.repositoryWork(), RepositoryRefCompatibility)
}

func (service *Service) historyRecorder() *historyRecorder {
	return historyCommandsForWork(service.meta, service.blobs, service.engine, service.repositories, service.repositoryWork(), HistoryCompatibility)
}
