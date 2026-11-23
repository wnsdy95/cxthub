package serverruntime

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ApplicationWorkerConfig struct {
	// GitReader reads the remote of a repository: its branches, commits and pull requests.
	GitReader          app.RemoteGitProvider
	GitHubWorker       *app.GitHubWorker
	InvitationSender   outbound.InvitationMailer
	NotificationSender outbound.NotificationSender
}

func newApplicationWorkers(storage store.Store, config ApplicationWorkerConfig) ([]workerBinding, error) {
	if _, production := storage.(outbound.ProductionStore); !production {
		return nil, fmt.Errorf("%w: application workers require production storage capabilities", domain.ErrCatalogUnsupported)
	}
	if config.GitReader == nil {
		return nil, fmt.Errorf("%w: application workers require a reader of the remote Git", domain.ErrValidation)
	}
	remoteGitSync, err := app.NewRemoteGitSync(app.RemoteGitSyncConfig{
		Metadata: storage, Blobs: storage, Repositories: storage, Reader: config.GitReader,
	})
	if err != nil {
		return nil, fmt.Errorf("configure remote Git worker: %w", err)
	}
	notificationWorker, err := app.NewNotificationWorker(app.NotificationWorkerConfig{
		Profile: notificationworker.Complete, Metadata: storage, Blobs: storage, Repositories: storage,
		Sender: config.NotificationSender,
	})
	if err != nil {
		return nil, fmt.Errorf("configure notification worker: %w", err)
	}
	invitationQueue, _ := storage.(outbound.InvitationEmailStore)
	invitations, _ := storage.(app.InvitationReader)
	invitationOrganizations, _ := storage.(app.InvitationOrganizationReader)
	invitationEnterprises, _ := storage.(app.InvitationEnterpriseReader)
	identityTransactions, _ := storage.(outbound.IdentityTransactions)
	invitationEmailWorker, err := app.NewInvitationEmailWorker(app.InvitationEmailWorkerConfig{
		Profile: app.InvitationEmailComplete, Queue: invitationQueue, Invitations: invitations,
		Organizations: invitationOrganizations, Enterprises: invitationEnterprises,
		Transactions: identityTransactions, Sender: config.InvitationSender,
	})
	if err != nil {
		return nil, fmt.Errorf("configure invitation email worker: %w", err)
	}
	maintenanceWorkers, err := app.NewMaintenanceWorkers(app.MaintenanceWorkersConfig{
		Profile: systemmaintenance.Complete, Repositories: storage,
	})
	if err != nil {
		return nil, fmt.Errorf("configure maintenance workers: %w", err)
	}
	var workers []workerBinding
	if config.GitHubWorker != nil {
		workers = append(workers, workerBinding{name: "github", run: config.GitHubWorker.Run})
	}
	return append(workers, []workerBinding{
		{name: "remote-git", run: remoteGitSync.Run},
		{name: "notification", run: notificationWorker.Run},
		{name: "invitation-email", run: invitationEmailWorker.Run},
		{name: "runtime-maintenance", run: maintenanceWorkers.Runtime.Run},
		{name: "storage-maintenance", run: maintenanceWorkers.Storage.Run},
	}...), nil
}
