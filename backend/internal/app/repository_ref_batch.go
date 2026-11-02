package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (commands *repositoryRefCommands) UpdateRefs(ctx context.Context, input inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	if commands == nil {
		return inbound.UpdateRefsOutput{}, domain.ErrValidation
	}
	if commands.configurationError != nil {
		return inbound.UpdateRefsOutput{}, commands.configurationError
	}
	output, err := commands.scope.WithinRefsWrite(ctx, input.RepoID, len(input.Updates) == 0, func(bound context.Context) (inbound.UpdateRefsOutput, error) {
		return commands.updateRefs(bound, input)
	})
	if err != nil && commands.profile == RepositoryRefComplete {
		return inbound.UpdateRefsOutput{}, err
	}
	return output, err
}

func (commands *repositoryRefCommands) updateRefs(ctx context.Context, in inbound.UpdateRefsInput) (out inbound.UpdateRefsOutput, err error) {
	if commands == nil {
		return out, domain.ErrValidation
	}
	if commands.configurationError != nil {
		return out, commands.configurationError
	}
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return out, err
	}
	if len(in.Updates) > inbound.MaxRefBatchUpdates {
		return out, fmt.Errorf("%w: at most %d ref updates per batch", domain.ErrValidation, inbound.MaxRefBatchUpdates)
	}
	defer func() {
		if err == nil {
			err = commands.reconcileSharedPendingPointers(ctx, in.RepoID)
		}
	}()
	var rejected []string
	for _, update := range in.Updates {
		update.RepoID = in.RepoID
		if _, updateErr := commands.updateRef(ctx, update); updateErr != nil {
			if errors.Is(updateErr, domain.ErrNonFastForward) {
				rejected = append(rejected, string(update.Ref.Kind)+"/"+update.Ref.Name)
				continue
			}
			return out, updateErr
		}
		out.Applied++
	}
	if len(rejected) > 0 {
		return out, fmt.Errorf("%w: %s", domain.ErrNonFastForward, strings.Join(rejected, ", "))
	}
	return out, nil
}
