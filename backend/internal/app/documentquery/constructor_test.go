package documentquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var constructorPrefixUnexpectedCall = errors.New("document query constructor invoked a port")

type constructorPrefixCallLog struct {
	methods []string
}

type constructorPrefixPorts struct {
	name string
	log  *constructorPrefixCallLog
}

func (ports *constructorPrefixPorts) called(method string) error {
	if ports == nil {
		panic("document query constructor invoked a typed-nil port: " + method)
	}
	ports.log.methods = append(ports.log.methods, ports.name+"."+method)
	return constructorPrefixUnexpectedCall
}

func (ports *constructorPrefixPorts) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ports.called("AuthorizeDocumentRead")
}

func (ports *constructorPrefixPorts) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ports.called("AuthorizeDocumentPreparation")
}

func (ports *constructorPrefixPorts) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	return ports.called("WithinDocumentRead")
}

func (ports *constructorPrefixPorts) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	return ports.called("WithinDocumentSnapshot")
}

func (ports *constructorPrefixPorts) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{}, ports.called("GetSnapshot")
}

func (ports *constructorPrefixPorts) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, ports.called("GetDoc")
}

func (ports *constructorPrefixPorts) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, ports.called("GetDocManifest")
}

func (ports *constructorPrefixPorts) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, ports.called("GetChunk")
}

func (ports *constructorPrefixPorts) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, ports.called("DocReadIndex")
}

func (ports *constructorPrefixPorts) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, ports.called("SearchDocEvents")
}

func (ports *constructorPrefixPorts) ReadVerifiedDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	return domain.VerifiedSessionDoc{}, ports.called("ReadVerifiedDoc")
}
