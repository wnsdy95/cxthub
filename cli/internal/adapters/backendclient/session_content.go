package backendclient

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// SessionContentUploader stores a session's content document through the
// existing chunked document path: it negotiates, sends only the chunks the
// server lacks, and has the server verify and finalize the document. It
// publishes no snapshot, ref or pending context, and keeps nothing on disk.
type SessionContentUploader struct {
	client   *BackendClient
	identity func(context.Context) (domain.DocumentIdentity, error)
}

// NewSessionContentUploader creates the uploader. identity answers which
// document identity the repository captures with, as capture itself decides.
func NewSessionContentUploader(client *BackendClient, identity func(context.Context) (domain.DocumentIdentity, error)) *SessionContentUploader {
	return &SessionContentUploader{client: client, identity: identity}
}

var _ outbound.SessionContentRemote = (*SessionContentUploader)(nil)

// PutSessionContent uploads doc and returns its reference.
func (u *SessionContentUploader) PutSessionContent(ctx context.Context, repoID string, doc domain.CIRDocument) (domain.DocumentRef, error) {
	if u == nil || u.client == nil || u.identity == nil {
		return domain.DocumentRef{}, fmt.Errorf("session content upload unavailable")
	}
	if len(doc.Events) == 0 {
		return domain.DocumentRef{}, fmt.Errorf("%w: a session content document needs at least one event", domain.ErrInvalidCIR)
	}
	identity, err := u.identity(ctx)
	if err != nil {
		return domain.DocumentRef{}, err
	}
	var chunks outbound.DocumentChunks
	var bodies map[domain.ContentHash][]byte
	switch identity {
	case domain.DocumentIdentityRootV1:
		manifest, built, err := domain.ConversationManifestForCIR(doc)
		if err != nil {
			return domain.DocumentRef{}, err
		}
		hash, err := domain.ConversationManifestHash(manifest)
		if err != nil {
			return domain.DocumentRef{}, err
		}
		canonical, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			return domain.DocumentRef{}, err
		}
		bodies = built
		chunks.Representation = domain.DocumentRepresentation{Hash: hash, Identity: identity, RootManifest: canonical}
	case domain.DocumentIdentityLegacy:
		canonical, err := domain.CanonicalBytes(doc)
		if err != nil {
			return domain.DocumentRef{}, err
		}
		plan, ok := chunkcas.PlanDoc(canonical)
		if !ok {
			return domain.DocumentRef{}, fmt.Errorf("%w: session content cannot be chunked", domain.ErrInvalidCIR)
		}
		bodies = plan.Bodies
		chunks.Representation = domain.DocumentRepresentation{Hash: domain.HashContent(canonical), Format: plan.Manifest.Format,
			Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	default:
		return domain.DocumentRef{}, domain.ErrUnsupportedDocumentIdentity
	}
	chunks.ReadChunk = func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		body, ok := bodies[hash]
		if !ok {
			return nil, domain.ErrNotFound
		}
		return body, nil
	}
	uploaded, err := u.client.PushDocChunks(ctx, repoID, chunks)
	if err != nil {
		return domain.DocumentRef{}, err
	}
	if !uploaded {
		return domain.DocumentRef{}, fmt.Errorf("%w: the server does not take chunked documents, or the document is too large for one manifest", domain.ErrSessionContentNotAccepted)
	}
	return chunks.Representation.DocumentRef(), nil
}
