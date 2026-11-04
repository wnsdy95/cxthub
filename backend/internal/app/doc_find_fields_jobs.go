package app

import (
	"context"
	"io"
	"log"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigureDocumentFindFields(storage outbound.DocFindFieldsStore) {
	service.docFindFields = storage
}

type docFindEventReader struct {
	source docViewSource
	entry  domain.DocEventIndex
	next   int
}

func (reader *docFindEventReader) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	if reader.next == reader.entry.Length {
		return 0, io.EOF
	}
	length := min(len(target), docViewBytes, reader.entry.Length-reader.next)
	raw, err := reader.source.read(reader.entry.Offset+reader.next, length)
	if err != nil {
		return 0, err
	}
	if len(raw) != length {
		return 0, domain.ErrIntegrity
	}
	reader.next += copy(target, raw)
	return length, nil
}

func (service *Service) buildDocFindFields(ctx context.Context, job domain.DocRangeIndexJob) error {
	worker, err := service.documentFindFieldsWorker()
	if err != nil {
		return err
	}
	return worker.Build(ctx, job)
}

func (service *Service) runDocFindFieldsJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	worker, err := service.documentFindFieldsWorker()
	if err != nil {
		return err
	}
	return worker.RunJob(ctx, job)
}

func (service *Service) ProcessDocFindFieldsJobs(ctx context.Context, limit int) error {
	if service.docFindFields == nil || limit <= 0 {
		return nil
	}
	worker, err := service.documentFindFieldsWorker()
	if err != nil {
		return err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) RunDocFindFieldsWorker(ctx context.Context) {
	if service.docFindFields == nil {
		return
	}
	worker, err := service.documentFindFieldsWorker()
	if err != nil {
		log.Printf("document find fields: %v", err)
		return
	}
	worker.Run(ctx)
}
