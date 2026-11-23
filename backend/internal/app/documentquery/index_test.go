package documentquery

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type indexOwnerPorts struct {
	test      *testing.T
	ctx       context.Context
	repo      domain.ContentHash
	hash      domain.ContentHash
	snapshot  domain.Snapshot
	document  domain.SessionDoc
	index     domain.DocReadIndex
	verified  domain.VerifiedSessionDoc
	calls     []string
	failStage string
	failure   error
}

func (ports *indexOwnerPorts) call(ctx context.Context, stage string, repo, hash domain.ContentHash) error {
	ports.test.Helper()
	if ctx != ports.ctx || repo != ports.repo || hash != "" && hash != ports.hash {
		ports.test.Fatal("index delegation replaced the caller's pinned context or exact repository/document identity")
	}
	ports.calls = append(ports.calls, stage)
	if err := ctx.Err(); err != nil {
		return err
	}
	if stage == ports.failStage {
		return ports.failure
	}
	return nil
}

func (ports *indexOwnerPorts) AuthorizeDocumentRead(context.Context, domain.ContentHash) (context.Context, error) {
	ports.test.Fatal("index delegation repeated document authorization")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) AuthorizeDocumentPreparation(context.Context, domain.ContentHash) (context.Context, error) {
	ports.test.Fatal("index delegation attempted preparation")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	ports.test.Fatal("index delegation acquired a second document admission")
	return domain.ErrIntegrity
}

func (ports *indexOwnerPorts) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if err := ports.call(ctx, "snapshot", repo, ""); err != nil {
		return err
	}
	return read(ctx)
}

func (ports *indexOwnerPorts) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	err := ports.call(ctx, "metadata", repo, hash)
	return ports.snapshot, err
}

func (ports *indexOwnerPorts) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	err := ports.call(ctx, "document", repo, hash)
	return ports.document, err
}

func (ports *indexOwnerPorts) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	ports.test.Fatal("index metadata read fetched a chunk manifest")
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	ports.test.Fatal("index metadata read fetched a payload chunk")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	err := ports.call(ctx, "index", repo, hash)
	return ports.index, err
}

func (ports *indexOwnerPorts) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	ports.test.Fatal("index delegation attempted a search")
	return nil, domain.ErrIntegrity
}

func (ports *indexOwnerPorts) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	err := ports.call(ctx, "verified", repo, hash)
	return ports.verified, err
}
