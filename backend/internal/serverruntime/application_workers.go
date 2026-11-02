package serverruntime

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ApplicationWorkerConfig struct {
	PendingRepositories       []domain.ContentHash
	GraphRepositories         []domain.ContentHash
	QualificationRepositories []domain.ContentHash
	MemoryRepositories        []domain.ContentHash
	GitReader                 outbound.GitEvidenceReader
	GitHubWorker              *app.GitHubWorker
	InvitationSender          outbound.InvitationMailer
	NotificationSender        outbound.NotificationSender
}

func newApplicationWorkers(storage store.Store, config ApplicationWorkerConfig) ([]workerBinding, error) {
	if _, production := storage.(outbound.ProductionStore); !production {
		return nil, fmt.Errorf("%w: application workers require production storage capabilities", domain.ErrCatalogUnsupported)
	}
	if config.GitReader == nil {
		return nil, fmt.Errorf("%w: application workers require a Git evidence reader", domain.ErrValidation)
	}
	commitReader, supportsCommits := config.GitReader.(outbound.GitCommitReader)
	if !supportsCommits {
		return nil, fmt.Errorf("%w: application worker Git reader must support commit discovery", domain.ErrValidation)
	}
	var authorizer outbound.GitSourceAuthorizer
	if config.GitHubWorker != nil {
		authorizer = config.GitHubWorker
	}
	gitChangeWorker, err := app.NewGitChangeWorker(app.GitChangeWorkerConfig{
		Profile: gitchangeworker.Complete, Metadata: storage, Blobs: storage, Repositories: storage,
		Reader: config.GitReader, Authorizer: authorizer,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Git change worker: %w", err)
	}
	gitIntegrationWorkers, err := app.NewGitIntegrationWorkers(app.GitIntegrationWorkerConfig{
		Profile: app.GitIntegrationComplete, Metadata: storage, Blobs: storage, Repositories: storage,
		Reader: commitReader, Authorizer: authorizer,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Git integration workers: %w", err)
	}
	projectionWorkers, err := app.NewProjectionWorkers(app.ProjectionWorkersConfig{
		Profile: projectionworker.Complete, Metadata: storage, Blobs: storage,
		PendingEnabled: len(config.PendingRepositories) != 0, GraphEnabled: len(config.GraphRepositories) != 0,
		QualificationRepositories: config.QualificationRepositories,
	})
	if err != nil {
		return nil, fmt.Errorf("configure projection workers: %w", err)
	}
	if projectionWorkers.Pending == nil || projectionWorkers.Graph == nil || projectionWorkers.Qualification == nil {
		return nil, fmt.Errorf("%w: application workers require pending, graph and qualification storage capabilities", domain.ErrCatalogUnsupported)
	}
	memoryResources, err := app.NewMemoryPreparationResources(app.MemoryPreparationResourceConfig{
		Metadata: storage, Blobs: storage, Repositories: storage,
	})
	if err != nil {
		return nil, fmt.Errorf("configure memory preparation resources: %w", err)
	}
	memoryPreparations, err := app.NewMemoryPreparationWorkers(app.MemoryPreparationWorkerConfig{
		Profile: app.MemoryPreparationComplete, Resources: memoryResources, Repositories: config.MemoryRepositories,
	})
	if err != nil {
		return nil, fmt.Errorf("configure memory preparation workers: %w", err)
	}
	prPromotionWorker, err := app.NewPRPromotionWorker(app.PRPromotionWorkerConfig{
		Profile: app.PRPromotionComplete, Metadata: storage, Blobs: storage, Repositories: storage,
		Engine: gitengine.NewEngine(storage), Evidence: storage,
	})
	if err != nil {
		return nil, fmt.Errorf("configure PR promotion worker: %w", err)
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
		{name: "git-change", run: gitChangeWorker.Run},
		{name: "git-integration", run: gitIntegrationWorkers.Run},
		{name: "git-reconciler", run: gitIntegrationWorkers.RunReconciler},
		{name: "pending-projection", run: projectionWorkers.Pending.Run},
		{name: "graph-projection", run: projectionWorkers.Graph.Run},
		{name: "memory-preparation", run: memoryPreparations.Run},
		{name: "graph-qualification", run: projectionWorkers.Qualification.Run},
		{name: "pr-promotion", run: prPromotionWorker.Run},
		{name: "notification", run: notificationWorker.Run},
		{name: "invitation-email", run: invitationEmailWorker.Run},
		{name: "runtime-maintenance", run: maintenanceWorkers.Runtime.Run},
		{name: "storage-maintenance", run: maintenanceWorkers.Storage.Run},
	}...), nil
}
