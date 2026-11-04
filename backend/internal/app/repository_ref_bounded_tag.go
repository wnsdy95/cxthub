package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryRefBoundedTagStore interface {
	HasOrdinaryTagTarget(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
}

func (service *Service) SupportsBoundedTagWrites() bool {
	return service != nil && service.repositoryRefs().SupportsBoundedTagWrites()
}

func (commands *repositoryRefCommands) SupportsBoundedTagWrites() bool {
	return commands != nil && commands.configurationError == nil && commands.boundedTags != nil
}

func (commands *repositoryRefCommands) checkRefTarget(ctx context.Context, repo domain.ContentHash, ref domain.Ref) error {
	var err error
	if commands.boundedTags != nil && ref.Kind == domain.RefTag && !strings.HasPrefix(ref.Name, "cxt/") {
		var present bool
		present, err = commands.boundedTags.HasOrdinaryTagTarget(ctx, repo, ref.Target)
		if err == nil && !present {
			err = domain.ErrNotFound
		}
	} else {
		_, err = commands.meta.GetSnapshot(ctx, repo, ref.Target)
	}
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: ref target snapshot %s does not exist", domain.ErrIntegrity, ref.Target)
	}
	return err
}
