package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func prepareCapturedMemoryRead(ctx context.Context, captured *CapturedEffectiveMemory, base *memoryGraphAnchor, delta *memoryGraphDelta) (*PreparedMemoryRead, error) {
	witnesses, err := captured.ObjectWitnesses(ctx)
	if err != nil {
		return nil, err
	}
	input, err := captured.exportWithGraphBase(ctx, base, delta)
	if err != nil {
		return nil, err
	}
	output, err := buildCapturedMemoryRead(ctx, captured, input.ID)
	if err != nil {
		return nil, err
	}
	qualification := domain.MemoryReadQualification{Version: domain.MemoryReadQualificationVersion, ReadID: output.ID,
		CaptureID: input.ID, Source: captured.generation, Witnesses: witnesses}
	if err := domain.ValidateMemoryReadQualification(ctx, qualification, output.Header); err != nil {
		return nil, err
	}
	if input.Header.Version == domain.MemoryCaptureVersion {
		base = &memoryGraphAnchor{id: input.ID, graph: captured.graph}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &PreparedMemoryRead{input: input, output: output, qualification: qualification, graphAnchor: base}, nil
}

func (workers *MemoryPreparationWorkers) PrepareIncrementalMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, expiresAt time.Time, previous *PreparedMemoryRead) (*PreparedMemoryRead, error) {
	return workers.prepareIncrementalMemoryRead(ctx, repo, request, expiresAt, previous, false)
}

func (workers *MemoryPreparationWorkers) prepareIncrementalMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, expiresAt time.Time, previous *PreparedMemoryRead, compare bool) (*PreparedMemoryRead, error) {
	if _, system := inbound.RepositoryActor(ctx); !system || inbound.DocumentReadAuthorizer(ctx) != nil {
		return nil, domain.ErrForbidden
	}
	transactions, known := workers.meta.(outbound.RepositoryTransactionPresence)
	if !known {
		return nil, domain.ErrCatalogUnsupported
	}
	if transactions.InTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	if !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(24*time.Hour)) {
		return nil, domain.ErrValidation
	}
	var prepared *PreparedMemoryRead
	err := workers.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		bound, cancel := context.WithTimeout(bound, 30*time.Second)
		defer cancel()
		base, err := workers.findMemoryGraphAnchor(bound, repo, expiresAt, previous)
		if err != nil {
			return err
		}
		var graph *CapturedGraphSource
		var delta *memoryGraphDelta
		if base != nil {
			graph, delta = &base.graph, &memoryGraphDelta{}
		}
		var expected domain.ContentHash
		var reference func(context.Context) error
		if compare {
			reference = func(snapshot context.Context) error {
				var err error
				expected, err = workers.memoryReadReference(snapshot, repo, request)
				return err
			}
		}
		captured, err := workers.captureEffectiveMemorySource(bound, repo, request, graph, delta, reference)
		if base != nil && (errors.Is(err, domain.ErrCatalogResetRequired) || errors.Is(err, domain.ErrRepositoryViewSourceLimit)) {
			base, delta = nil, nil
			captured, err = workers.captureEffectiveMemorySource(bound, repo, request, nil, nil, reference)
		}
		if err != nil {
			return err
		}
		prepared, err = prepareCapturedMemoryRead(bound, captured, base, delta)
		if err == nil && compare {
			prepared.comparison, err = newMemoryReadComparison(bound, prepared.output, expected)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (workers *MemoryPreparationWorkers) findMemoryGraphAnchor(ctx context.Context, repo domain.ContentHash, expiresAt time.Time, previous *PreparedMemoryRead) (*memoryGraphAnchor, error) {
	anchors, supported := workers.meta.(outbound.MemoryCaptureAnchorStore)
	captures, captureOK := workers.meta.(outbound.MemoryCaptureStore)
	factory, factoryOK := workers.meta.(outbound.GraphSourceReaderFactory)
	if !supported || !captureOK || !factoryOK {
		return nil, domain.ErrCatalogUnsupported
	}
	identifier, err := memoryPreparationReadForRepo(ctx, workers, repo, func(bound context.Context) (domain.ContentHash, error) {
		_, generation, err := workers.memorySourceGeneration(bound, repo)
		if err != nil {
			return "", err
		}
		if generation == nil {
			return "", domain.ErrCatalogUnsupported
		}
		return anchors.FindMemoryCaptureAnchor(bound, generation.Graph, expiresAt)
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.graphAnchor != nil && previous.graphAnchor.id == identifier && previous.graphAnchor.graph.checkpoint.RepoID == repo {
		return previous.graphAnchor, nil
	}
	bundle, err := captures.ReadMemoryCapture(ctx, repo, identifier)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrMemoryCaptureExpired) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if bundle.ID != identifier || bundle.Header.Version != domain.MemoryCaptureVersion || bundle.Header.Generation.Graph.RepoID != repo {
		return nil, domain.ErrIntegrity
	}
	captured, err := RestoreCapturedEffectiveMemory(ctx, bundle, factory)
	if err != nil {
		return nil, err
	}
	return &memoryGraphAnchor{id: identifier, graph: captured.graph}, nil
}
