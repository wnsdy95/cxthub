package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type NotificationWorkerConfig struct {
	Profile      notificationworker.Profile
	Metadata     outbound.MetadataStore
	Blobs        outbound.BlobStore
	Repositories outbound.RepositoryStore
	Sender       outbound.NotificationSender
}

type notificationWorkerScope struct {
	supported []domain.DocumentIdentity
}

func (scope notificationWorkerScope) WorkerContext(ctx context.Context) context.Context {
	return outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(ctx, scope.supported), scope.supported, scope.supported)
}

func NewNotificationWorker(config NotificationWorkerConfig) (*notificationworker.Worker, error) {
	if !rootAdapterPresent(config.Metadata) || (config.Blobs != nil || config.Profile == notificationworker.Complete) && !rootAdapterPresent(config.Blobs) {
		return nil, domain.ErrValidation
	}
	jobs, _ := config.Metadata.(outbound.NotificationStore)
	guard, _ := config.Metadata.(outbound.NotificationDeliveryGuard)
	return notificationworker.New(notificationworker.Config{
		Profile: config.Profile, Scope: notificationWorkerScope{supported: ReadDocumentIdentities(config.Metadata, config.Blobs)},
		Jobs: jobs, Repositories: config.Repositories, Guard: guard, Sender: config.Sender,
	})
}

func (service *Service) ConfigureNotificationSender(sender outbound.NotificationSender) error {
	if !rootAdapterPresent(sender) {
		return domain.ErrValidation
	}
	service.notificationSender = sender
	return nil
}

func (service *Service) notificationWorker(profile notificationworker.Profile) (*notificationworker.Worker, error) {
	return NewNotificationWorker(NotificationWorkerConfig{
		Profile: profile, Metadata: service.meta, Blobs: service.blobs,
		Repositories: service.repositories, Sender: service.notificationSender,
	})
}

type MaintenanceWorkersConfig struct {
	Profile      systemmaintenance.Profile
	Repositories outbound.RepositoryStore
}

type MaintenanceWorkers struct {
	Runtime *systemmaintenance.Runtime
	Storage *systemmaintenance.Storage
}

func NewMaintenanceWorkers(config MaintenanceWorkersConfig) (*MaintenanceWorkers, error) {
	if !rootAdapterPresent(config.Repositories) {
		return nil, domain.ErrValidation
	}
	runtimeStore, _ := config.Repositories.(systemmaintenance.RuntimeStore)
	storageStore, _ := config.Repositories.(systemmaintenance.StorageStore)
	if config.Profile == systemmaintenance.Complete && (!rootAdapterPresent(runtimeStore) || !rootAdapterPresent(storageStore)) {
		return nil, domain.ErrCatalogUnsupported
	}
	runtime, err := systemmaintenance.NewRuntime(systemmaintenance.RuntimeConfig{Profile: config.Profile, Store: runtimeStore})
	if err != nil {
		return nil, err
	}
	storage, err := systemmaintenance.NewStorage(systemmaintenance.StorageConfig{Profile: config.Profile, Store: storageStore})
	if err != nil {
		return nil, err
	}
	return &MaintenanceWorkers{Runtime: runtime, Storage: storage}, nil
}
