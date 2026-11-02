package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"reflect"
)

// Main must review all publication, import, read and worker gates before this
// release fuse can change. The environment cannot override incomplete code.
const conversationRootReleaseReady = true

func ValidateConversationRootPublicationConfig(enabled bool) error {
	if enabled && !conversationRootReleaseReady {
		return domain.ErrRootPublicationDisabled
	}
	return nil
}
func (s *Service) ConfigureConversationRootPublication(enabled bool) error {
	if err := ValidateConversationRootPublicationConfig(enabled); err != nil {
		return err
	}
	if enabled && !s.rootPublicationAdaptersAvailable() {
		return domain.ErrRootPublicationDisabled
	}
	s.rootPublication = enabled
	return nil
}
func (s *Service) DocumentIdentitiesSupported() []domain.DocumentIdentity {
	if s == nil {
		return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
	}
	return ReadDocumentIdentities(s.meta, s.blobs)
}

func ReadDocumentIdentities(metadata outbound.MetadataStore, blobs outbound.BlobStore) []domain.DocumentIdentity {
	if rootDocumentReadersAvailable(metadata, blobs) {
		return binaryDocumentIdentitiesSupported()
	}
	return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
}

// Identity-only administration checks the binary's code support; it does not
// advertise document service capabilities or admit document publication.
func binaryDocumentIdentitiesSupported() []domain.DocumentIdentity {
	if conversationRootReleaseReady {
		return []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	}
	return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
}
func (s *Service) RootPublicationEnabled() bool {
	return conversationRootReleaseReady && s.rootPublication && s.rootPublicationAdaptersAvailable()
}

// Admission off must retain complete readers and accepted-job recovery. FS has
// these ports but deliberately lacks production repository transactions.
func (s *Service) rootDocumentAdaptersAvailable() bool {
	return s != nil && rootDocumentReadersAvailable(s.meta, s.blobs)
}

func rootDocumentReadersAvailable(metadata outbound.MetadataStore, blobs outbound.BlobStore) bool {
	return rootAdapterPresent(metadata) && rootAdapterPresent(blobs) &&
		supports[outbound.RepositoryDocumentIdentityStore](metadata) &&
		supports[outbound.DocJobStore](blobs) &&
		supports[outbound.StoredDocVerifier](blobs) &&
		supports[outbound.VerifiedDocReader](blobs) &&
		supports[outbound.ConversationChunkReader](blobs)
}

func (s *Service) rootPublicationAdaptersAvailable() bool {
	return s.rootDocumentAdaptersAvailable() && supports[outbound.RepositoryTransactions](s.meta)
}

func rootAdapterPresent(adapter any) bool {
	if adapter == nil {
		return false
	}
	switch value := reflect.ValueOf(adapter); value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return !value.IsNil()
	}
	return true
}
func (s *Service) DocumentIdentityWorkerContext(ctx context.Context) context.Context {
	return inbound.WithDocumentIdentities(ctx, s.DocumentIdentitiesSupported())
}
func hasDocumentIdentity(ids []domain.DocumentIdentity, required domain.DocumentIdentity) bool {
	if required == domain.DocumentIdentityLegacy {
		return true
	}
	for _, id := range ids {
		if id == required {
			return true
		}
	}
	return false
}
func (s *Service) checkDocumentIdentity(ctx context.Context, repo domain.ContentHash, allowMissing bool) error {
	return checkRepositoryDocumentIdentity(ctx, s.meta, s.DocumentIdentitiesSupported(), repo, allowMissing)
}

func checkRepositoryDocumentIdentity(ctx context.Context, metadata documentQueryRepoReader, supported []domain.DocumentIdentity, repo domain.ContentHash, allowMissing bool) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range inbound.DocumentIdentities(ctx) {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	r, err := metadata.GetRepo(ctx, repo)
	if allowMissing && errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = r.RequiredDocIdentity.Validate(); err != nil {
		return err
	}
	if !hasDocumentIdentity(supported, r.RequiredDocIdentity) || !hasDocumentIdentity(inbound.DocumentIdentities(ctx), r.RequiredDocIdentity) {
		return domain.ErrDocumentIdentityUpgradeRequired
	}
	return nil
}

// Reuse the existing coherent read helper; the requirement is read from exactly
// the same snapshot as the payload. This does not pretend to cover other APIs.
func repositoryReadForRepo[T any](ctx context.Context, s *Service, repo domain.ContentHash, fn func(context.Context) (T, error)) (T, error) {
	return repositoryReadForRepoWithWork(ctx, s.repositoryWork(), repo, fn)
}
