package app

func (service *Service) MemoryPreparationResources() *MemoryPreparationResources {
	return &MemoryPreparationResources{meta: service.meta, blobs: service.blobs, work: service.repositoryWork(),
		scope: service.memorySnapshotReadScope(), delivery: &service.deliveryCache,
		projectionMemory: service.projectionMemory, projectionEvidence: service.projectionEvidence,
		projectionMemorySource: service.projectionMemorySource}
}

func (service *Service) memoryPreparation() *MemoryPreparationWorkers {
	return &MemoryPreparationWorkers{MemoryPreparationResources: service.MemoryPreparationResources(), memoryReadRepos: service.memoryReadRepos}
}
