package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

func (ports *constructorPrefixPorts) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocRangeIndex")
}

func (ports *constructorPrefixPorts) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocVisibleIndex")
}

func (ports *constructorPrefixPorts) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	return 0, ports.called("DocRangeBoundary")
}

func (ports *constructorPrefixPorts) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, ports.called("ReadConversationChunk")
}

func (ports *constructorPrefixPorts) LegacyByteRangePublicationEnabled() bool {
	_ = ports.called("LegacyByteRangePublicationEnabled")
	return false
}

func (ports *constructorPrefixPorts) HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocByteRangeIndex")
}
