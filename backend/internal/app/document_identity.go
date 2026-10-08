package app

import (
	"context"
	"errors"
	"fmt"
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
	if s.rootDocumentAdaptersAvailable() {
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
	return s != nil && rootAdapterPresent(s.meta) && rootAdapterPresent(s.blobs) &&
		supports[outbound.RepositoryDocumentIdentityStore](s.meta) &&
		supports[outbound.DocJobStore](s.blobs) &&
		supports[outbound.StoredDocVerifier](s.blobs) &&
		supports[outbound.VerifiedDocReader](s.blobs) &&
		supports[outbound.ConversationChunkReader](s.blobs)
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
	r, err := s.meta.GetRepo(ctx, repo)
	if allowMissing && errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = r.RequiredDocIdentity.Validate(); err != nil {
		return err
	}
	if !hasDocumentIdentity(s.DocumentIdentitiesSupported(), r.RequiredDocIdentity) || !hasDocumentIdentity(inbound.DocumentIdentities(ctx), r.RequiredDocIdentity) {
		return domain.ErrDocumentIdentityUpgradeRequired
	}
	return nil
}

// Reuse the existing coherent read helper; the requirement is read from exactly
// the same snapshot as the payload. This does not pretend to cover other APIs.
func repositoryReadForRepo[T any](ctx context.Context, s *Service, repo domain.ContentHash, fn func(context.Context) (T, error)) (T, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
	return repositoryRead(ctx, s, func(bound context.Context) (T, error) {
		// Older local FS layouts can contain legacy objects before registration.
		// Missing metadata means no requirement; all other read failures fail closed.
		// PG keeps this decision in the payload snapshot and enforces repo FKs.
		if err := s.checkDocumentIdentity(bound, repo, true); err != nil {
			var zero T
			return zero, err
		}
		return fn(bound)
	})
}
func (s *Service) requireDocumentIdentityCommand(ctx context.Context, repo domain.ContentHash, next domain.DocumentIdentity) error {
	r, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err = domain.ValidateDocumentIdentityRequirement(r.RequiredDocIdentity, next); err != nil {
		return err
	}
	if next == domain.DocumentIdentityLegacy {
		return nil
	}
	if !s.RootPublicationEnabled() {
		return domain.ErrRootPublicationDisabled
	}
	if !hasDocumentIdentity(inbound.DocumentIdentities(ctx), next) || !hasDocumentIdentity(s.DocumentIdentitiesSupported(), next) {
		return domain.ErrDocumentIdentityUpgradeRequired
	}
	if _, ok := s.meta.(outbound.RepositoryTransactions); !ok {
		return fmt.Errorf("%w: repository transaction unavailable", domain.ErrRootPublicationDisabled)
	}
	st, ok := s.meta.(outbound.RepositoryDocumentIdentityStore)
	if !ok {
		return domain.ErrRootPublicationDisabled
	}
	return st.RequireDocumentIdentity(ctx, repo, next)
}
