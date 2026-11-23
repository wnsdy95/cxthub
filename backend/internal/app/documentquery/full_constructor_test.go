package documentquery

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (ports *constructorPrefixPorts) AcquireInteractive(context.Context) (context.Context, func(), error) {
	return nil, nil, ports.called("AcquireInteractive")
}

func (ports *constructorPrefixPorts) AcquireBulk(context.Context) (context.Context, func(), error) {
	return nil, nil, ports.called("AcquireBulk")
}

func (ports *constructorPrefixPorts) InReadOnlyTransaction(context.Context) bool {
	_ = ports.called("InReadOnlyTransaction")
	return false
}

func (ports *constructorPrefixPorts) DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error) {
	return domain.DocRangeIndex{}, ports.called("DocRangeIndex")
}

func (ports *constructorPrefixPorts) DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error) {
	return nil, ports.called("DocRangePage")
}

func (ports *constructorPrefixPorts) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	return ports.called("EnsureDocRangeIndex")
}

func (ports *constructorPrefixPorts) OpenDocRange(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeRead, error) {
	return nil, ports.called("OpenDocRange")
}

func (ports *constructorPrefixPorts) OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	return nil, ports.called("OpenDocRangeGeneration")
}

func (ports *constructorPrefixPorts) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocRangeIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocRangeJob")
}

func (ports *constructorPrefixPorts) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocVisibleIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocVisibleIndex")
}

func (ports *constructorPrefixPorts) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	return 0, ports.called("DocRangeBoundary")
}

func (ports *constructorPrefixPorts) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocRangeBoundary")
}

func (ports *constructorPrefixPorts) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, ports.called("ReadConversationChunk")
}

func (ports *constructorPrefixPorts) ReadLegacyDerivedByteChunk(context.Context, domain.ContentHash, domain.ContentHash, domain.DocReadGenerationBinding, int, domain.ConversationManifestChunk) ([]byte, error) {
	return nil, ports.called("ReadLegacyDerivedByteChunk")
}

func (ports *constructorPrefixPorts) LegacyByteRangePublicationEnabled() bool {
	_ = ports.called("LegacyByteRangePublicationEnabled")
	return false
}

func (ports *constructorPrefixPorts) HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocByteRangeIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocByteRangeJob")
}

func (ports *constructorPrefixPorts) ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error) {
	return domain.DocFindFields{}, ports.called("ReadDocFindFields")
}

func (ports *constructorPrefixPorts) EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocFindFields")
}

func (ports *constructorPrefixPorts) ReadDocDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return domain.DocDisplayStringGeneration{}, ports.called("ReadDocDisplayGeneration")
}

func (ports *constructorPrefixPorts) ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, ports.called("ReadDocDisplayPage")
}

func (ports *constructorPrefixPorts) EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error) {
	return domain.DocDisplayJob{}, ports.called("EnqueueDocDisplay")
}

func (ports *constructorPrefixPorts) ReadDocEventDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	return domain.DocDisplayProjectionGeneration{}, ports.called("ReadDocEventDisplayGeneration")
}

func (ports *constructorPrefixPorts) ReadDocEventDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, ports.called("ReadDocEventDisplayPage")
}
