package backendclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.RepositoryInitialization = (*BackendClient)(nil)

// ReadRepositoryInitialization recovers only immutable creation metadata. An
// accepted branch anchor is a separate receipt, never part of this response.
func (c *BackendClient) ReadRepositoryInitialization(ctx context.Context, repo string) (domain.RepositoryInitializationReceipt, error) {
	var receipt domain.RepositoryInitializationReceipt
	if err := domain.ValidateContentHash(repo); err != nil {
		return receipt, err
	}
	if err := c.do(ctx, http.MethodGet, c.reposPath(repo)+"/initialization", nil, &receipt); err != nil {
		return domain.RepositoryInitializationReceipt{}, initializationError(err)
	}
	if err := receipt.Validate(repo); err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	if receipt.Anchor != nil {
		return domain.RepositoryInitializationReceipt{}, domain.ErrHashMismatch
	}
	return receipt, nil
}

func (c *BackendClient) BeginRepositoryInitialization(ctx context.Context, repo domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	var receipt domain.RepositoryInitializationReceipt
	if domain.ValidateContentHash(repo.ID) != nil || domain.ValidateBranchName(repo.DefaultBranch) != nil {
		return receipt, domain.ErrInvalidRef
	}
	in := domain.RepositoryInitializationRequest{RemoteURL: repo.RemoteURL, GitRemoteURL: repo.GitRemoteURL, DefaultBranch: repo.DefaultBranch}
	if err := c.do(ctx, http.MethodPost, c.reposPath(repo.ID)+"/initialization", in, &receipt); err != nil {
		return domain.RepositoryInitializationReceipt{}, initializationError(err)
	}
	if err := receipt.Validate(repo.ID); err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	return receipt, nil
}

func (c *BackendClient) FinalizeRepositoryInitialization(ctx context.Context, repo string, in domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error) {
	var receipt domain.RepositoryInitializationReceipt
	if err := in.Validate(repo); err != nil {
		return receipt, err
	}
	if err := c.do(ctx, http.MethodPost, c.reposPath(repo)+"/initialization/finalize", in, &receipt); err != nil {
		return domain.RepositoryInitializationReceipt{}, initializationError(err)
	}
	if err := receipt.Validate(repo); err != nil {
		return domain.RepositoryInitializationReceipt{}, err
	}
	if receipt.CreationID != in.CreationID || receipt.Anchor == nil || !reflect.DeepEqual(*receipt.Anchor, in.Anchor) {
		return domain.RepositoryInitializationReceipt{}, domain.ErrHashMismatch
	}
	return receipt, nil
}

func initializationError(err error) error {
	var failure *HTTPError
	if errors.As(err, &failure) {
		if failure.Status == http.StatusConflict && failure.Code == "repository_initialization_conflict" {
			return fmt.Errorf("%w: %w", domain.ErrRepositoryInitializationConflict, err)
		}
		if failure.Status == http.StatusNotFound || failure.Status == http.StatusMethodNotAllowed ||
			(failure.Status == http.StatusNotImplemented && failure.Code == "repository_initialization_unsupported") {
			return fmt.Errorf("%w: %w", domain.ErrRepositoryInitializationUnsupported, err)
		}
	}
	return err
}

func (c *BackendClient) RepositoryInitializationState(ctx context.Context, repo, initialBranch string) (outbound.RepositoryInitializationState, error) {
	view, err := c.readRepository(ctx, repo, initialBranch)
	if err != nil {
		var failure *HTTPError
		if errors.As(err, &failure) && failure.Status == http.StatusNotFound && failure.Code == "not_found" {
			// Only an explicit absent repository permits the application to ask
			// for atomic creation; transport/auth failures remain distinct.
			err = fmt.Errorf("%w: %w", domain.ErrNotFound, err)
		}
		return outbound.RepositoryInitializationState{}, err
	}
	if view.InitialAnchorAvailable && view.ContextProtocol != 1 {
		return outbound.RepositoryInitializationState{}, domain.ErrHashMismatch
	}
	return outbound.RepositoryInitializationState{Repo: view.Repo, InitialAnchorAvailable: view.InitialAnchorAvailable}, nil
}
