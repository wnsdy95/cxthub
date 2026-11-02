package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigureGraphReads(repos []domain.ContentHash) error {
	if len(repos) == 0 {
		service.graphReadRepos = nil
		return nil
	}
	if _, ok := service.meta.(outbound.GraphServingStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	if _, ok := service.meta.(outbound.GraphProjectionRepairStore); !ok {
		return domain.ErrCatalogUnsupported
	}
	configured := make(map[domain.ContentHash]bool, len(repos))
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		configured[repo] = true
	}
	service.graphReadRepos = configured
	return nil
}
