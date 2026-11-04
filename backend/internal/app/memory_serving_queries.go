package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) MemoryServingQueries() (*memoryquery.ServingQueries, error) {
	scope := service.memorySnapshotReadScope()
	scope.resources.forceGeneral = true
	current, _ := service.meta.(outbound.CurrentMemoryReadStore)
	qualified, _ := service.meta.(outbound.MemoryReadQualificationReader)
	work, _ := service.meta.(memoryquery.WorkStatusReader)
	transactions, _ := service.meta.(outbound.RepositoryTransactionPresence)
	return memoryquery.NewServing(memoryquery.ServingConfig{Scope: scope, Current: current, Qualified: qualified,
		WorkStatus: work, Transactions: transactions, SupportedIdentities: service.DocumentIdentitiesSupported()})
}
