package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentRequirementReader interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type documentPublicationLegacyReader interface {
	GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error)
}

type documentPublicationIntegrityVerifier interface {
	VerifyIntegrity(context.Context, domain.Snapshot, domain.SessionDoc) error
}

type documentPublicationSources struct {
	metadata  documentRequirementReader
	stored    outbound.StoredDocVerifier
	legacy    documentPublicationLegacyReader
	integrity documentPublicationIntegrityVerifier
	supported []domain.DocumentIdentity
}

func documentPublicationSourcesFromStores(metadata documentRequirementReader, blobs outbound.BlobStore, engine outbound.GitEngine, supported []domain.DocumentIdentity) *documentPublicationSources {
	stored, _ := blobs.(outbound.StoredDocVerifier)
	return &documentPublicationSources{metadata: metadata, stored: stored, legacy: blobs, integrity: engine,
		supported: append([]domain.DocumentIdentity(nil), supported...)}
}

func (sources *documentPublicationSources) VerifySnapshotDocument(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	if err := snapshot.DocIdentity.Validate(); err != nil {
		return err
	}
	if snapshot.DocIdentity == domain.DocumentIdentityRootV1 {
		if !hasDocumentIdentity(sources.supported, snapshot.DocIdentity) {
			return domain.ErrUnsupportedDocumentIdentity
		}
		ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), sources.supported)
		if err := requireRootDocumentRepository(ctx, sources.metadata, repo); err != nil {
			return err
		}
	}
	return sources.verifyReference(ctx, repo, snapshot)
}

func (sources *documentPublicationSources) verifyReference(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	if err := snapshot.DocumentRef().Validate(); err != nil {
		return err
	}
	if snapshot.DocIdentity == domain.DocumentIdentityRootV1 && snapshot.RepoID != repo {
		return domain.ErrIntegrity
	}
	if snapshot.ID == "" || snapshot.DocHash == "" || snapshot.ID != snapshot.DocHash {
		return domain.ErrIntegrity
	}
	if sources.stored != nil {
		if !rootAdapterPresent(sources.stored) {
			return domain.ErrValidation
		}
		proof, err := sources.stored.VerifyStoredDoc(ctx, repo, snapshot.DocHash)
		if err != nil {
			return err
		}
		if proof.DocumentRef() != snapshot.DocumentRef() || !proof.Matches(snapshot) {
			return domain.ErrIntegrity
		}
		return nil
	}
	if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if sources.legacy == nil || sources.integrity == nil {
		return domain.ErrCatalogUnsupported
	}
	document, err := sources.legacy.GetDoc(ctx, repo, snapshot.DocHash)
	if err != nil {
		return err
	}
	return sources.integrity.VerifyIntegrity(ctx, snapshot, document)
}
