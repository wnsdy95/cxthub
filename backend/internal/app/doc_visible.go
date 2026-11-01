package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) prepareDocVisibleSource(ctx context.Context, repo, hash domain.ContentHash, requirement docRangeRequirement) (domain.DocumentIdentity, error) {
	var needs bool
	legacy := false
	identity := domain.DocumentIdentityRootV1
	bytes, byteEnabled := service.docBytePreparation()
	requireBytes := false
	err := resourceWork(ctx, ResourceInteractive, func(admitted context.Context) error {
		var err error
		admitted, err = authorizeVisibleRead(admitted, repo)
		if err != nil {
			return err
		}
		needs, err = repositoryReadForRepo(admitted, service, repo, func(read context.Context) (bool, error) {
			snapshot, err := service.meta.GetSnapshot(read, repo, hash)
			if err != nil {
				return false, err
			}
			if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
				return false, err
			}
			identity = snapshot.DocIdentity
			legacy = requirement != docRangeRootOnly && snapshot.DocIdentity == domain.DocumentIdentityLegacy
			if legacy {
				requireBytes = requirement == docRangeLegacyBytes && byteEnabled
				if requireBytes {
					ready, err := bytes.HasDocByteRangeIndex(read, repo, hash)
					return !ready, err
				}
				storage, ok := service.blobs.(outbound.DocRangeReadinessStore)
				if !ok {
					return false, domain.ErrDocVisibleProjectionUnsupported
				}
				ready, err := storage.HasDocRangeIndex(read, repo, hash)
				return !ready, err
			}
			if err := service.checkVisibleRootSnapshot(hash, snapshot); err != nil {
				return false, err
			}
			storage, ok := service.blobs.(outbound.DocVisiblePreparationStore)
			if !ok {
				return false, domain.ErrDocVisibleProjectionUnsupported
			}
			ready, err := storage.HasDocVisibleIndex(read, repo, hash)
			return !ready, err
		})
		return err
	})
	if err != nil || !needs {
		return identity, err
	}
	if outbound.DocReadOnly(ctx) {
		return identity, domain.ErrDocRangeIndexUnavailable
	}
	return identity, resourceWork(ctx, ResourceInteractive, func(admitted context.Context) error {
		admitted, err := authorizeVisibleRead(admitted, repo)
		if err != nil {
			return err
		}
		var job domain.DocRangeIndexJob
		if legacy {
			storage, ok := service.blobs.(outbound.DocRangeJobStore)
			if !ok {
				return domain.ErrDocRangeIndexUnavailable
			}
			if requireBytes {
				job, err = bytes.EnqueueDocByteRangeJob(admitted, repo, hash, time.Now().UTC())
			} else {
				job, err = storage.EnqueueDocRangeJob(admitted, repo, hash, time.Now().UTC())
			}
		} else {
			job, err = service.blobs.(outbound.DocVisiblePreparationStore).EnqueueDocVisibleIndex(admitted, repo, hash, time.Now().UTC())
		}
		if err != nil {
			return err
		}
		return docRangePreparationError(job)
	})
}

func authorizeVisibleRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	if _, system := inbound.RepositoryActor(ctx); system {
		return ctx, nil
	}
	return ctx, domain.ErrUnauthorized
}

func (service *Service) requireVisibleRoot(ctx context.Context, repo, hash domain.ContentHash) error {
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return err
	}
	return service.checkVisibleRootSnapshot(hash, snapshot)
}

func (service *Service) checkVisibleRootSnapshot(hash domain.ContentHash, snapshot domain.Snapshot) error {
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	if _, ok := service.blobs.(outbound.DocRangeGenerationStore); !ok {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	if _, ok := service.blobs.(outbound.DocVisibleGenerationStore); !ok {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	if _, ok := service.blobs.(outbound.DocRangeStore); !ok {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	if _, ok := service.blobs.(outbound.ConversationChunkReader); !ok {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	return nil
}

func visiblePageSelection(repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisibleSelector, bool, error) {
	if validateHashes(repo, hash) != nil {
		return domain.DocVisibleSelector{}, false, domain.ErrValidation
	}
	if request.Cursor != "" || request.Expand != "" {
		if request.Cursor != "" && request.Expand != "" || request.Base != "" || request.View != "" || request.Limit != 0 || request.Offset != 0 && request.Offset != -1 {
			return domain.DocVisibleSelector{}, false, domain.ErrValidation
		}
		token, purpose := request.Cursor, "units"
		if request.Expand != "" {
			token, purpose = request.Expand, "fold"
		}
		selector, err := domain.DecodeDocVisibleSelector(token)
		if err != nil || selector.Repo != repo || selector.Hash != hash || selector.Purpose != purpose {
			return domain.DocVisibleSelector{}, false, domain.ErrValidation
		}
		return selector, true, nil
	}
	if domain.ValidateOptionalContentHash(request.Base) != nil || request.View != "all" && request.View != "chat" && request.View != "prompts" ||
		request.Offset < -1 || request.Offset > domain.MaxConversationDocumentBytes || request.Limit < 0 || request.Limit > 100 {
		return domain.DocVisibleSelector{}, false, domain.ErrValidation
	}
	limit := request.Limit
	if limit == 0 {
		limit = 20
	}
	return domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo, Hash: hash, Base: request.Base, View: request.View, Start: request.Offset, Limit: limit}, false, nil
}

func (service *Service) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocVisiblePage{}, err
	}
	return queries.ReadDocVisiblePage(ctx, repo, hash, request)
}

func visibleSelectionGeneration(generation domain.DocVisibleGeneration, source domain.DocReadGenerationBinding, base domain.ContentHash, previous domain.DocReadGenerationBinding) (domain.ContentHash, error) {
	raw, err := generation.Encode()
	if err != nil {
		return "", err
	}
	if source.Validate() != nil || source.RangeGeneration != generation.RangeGeneration || base != "" && previous.Validate() != nil {
		return "", domain.ErrIntegrity
	}
	encoded, err := json.Marshal(struct {
		Projection string
		Descriptor domain.ContentHash
		Source     domain.DocReadGenerationBinding
		Base       domain.ContentHash
		Previous   domain.DocReadGenerationBinding
	}{domain.DocVisibleProjection, domain.HashContent(raw), source, base, previous})
	if err != nil {
		return "", err
	}
	return domain.HashContent(encoded), nil
}
