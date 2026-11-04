package app

import (
	"context"
	"log"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docDisplayJobSteps = documentworker.DisplayJobSteps

func (service *Service) ConfigureDocumentDisplay(storage outbound.DocDisplayStore) {
	service.docDisplay = storage
	service.docDisplayRead = storage
}

func (service *Service) ConfigureDocumentDisplayReads(storage outbound.DocDisplayReader) {
	service.docDisplayRead = storage
}

func (service *Service) docDisplaySource(ctx context.Context, target domain.DocDisplayTarget) (docViewSource, error) {
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return docViewSource{}, err
	}
	source, err := worker.Source(ctx, target)
	if err != nil {
		return docViewSource{}, err
	}
	return docViewSource{binding: source.Binding(), envelope: source.Envelope(), total: source.Total(), root: source.Root(),
		streamBytes: source.StreamBytes(), entry: source.Entry, location: source.Location, read: source.Read}, nil
}

func (service *Service) buildDocDisplay(ctx context.Context, job domain.DocDisplayJob) (bool, error) {
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return false, err
	}
	return worker.Build(ctx, job)
}

func (service *Service) advanceDocDisplayDiscovery(ctx context.Context, job domain.DocDisplayJob, progress domain.DocDisplayPreparation) (domain.DocDisplayPreparation, error) {
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return worker.AdvanceDiscovery(ctx, job, progress)
}

func (service *Service) runDocDisplayJob(ctx context.Context, job domain.DocDisplayJob) error {
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return err
	}
	return worker.RunJob(ctx, job)
}

func (service *Service) ProcessDocDisplayJobs(ctx context.Context, limit int) error {
	_, err := service.processDocDisplayJobs(ctx, limit)
	return err
}

func (service *Service) processDocDisplayJobs(ctx context.Context, limit int) (int, error) {
	if service.docDisplay == nil || limit <= 0 {
		return 0, nil
	}
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return 0, err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) RunDocDisplayWorker(ctx context.Context) {
	if service.docDisplay == nil {
		return
	}
	worker, err := service.documentDisplayWorker()
	if err != nil {
		log.Printf("document display preparation: %v", err)
		return
	}
	worker.Run(ctx)
}
