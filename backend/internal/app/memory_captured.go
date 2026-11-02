package app

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type CapturedEffectiveMemory struct {
	generation domain.RepositoryMemorySourceGeneration
	selection  domain.EffectiveMemorySelection
	content    string
	graph      CapturedGraphSource
	reader     *capturedMemoryReader
	evidence   outbound.GraphEvidenceReader
	witnesses  map[domain.ContentHash]domain.MemoryObjectWitness
}

type CapturedMemoryDependency struct {
	Hash      domain.ContentHash
	Available bool
}

func (captured *CapturedEffectiveMemory) Generation() domain.RepositoryMemorySourceGeneration {
	if captured == nil {
		return domain.RepositoryMemorySourceGeneration{}
	}
	return captured.generation
}

func (captured *CapturedEffectiveMemory) Dependencies() []CapturedMemoryDependency {
	if captured == nil || captured.reader == nil {
		return nil
	}
	dependencies := make([]CapturedMemoryDependency, 0, len(captured.reader.objects))
	for hash, raw := range captured.reader.objects {
		dependencies = append(dependencies, CapturedMemoryDependency{Hash: hash, Available: raw != nil})
	}
	slices.SortFunc(dependencies, func(left, right CapturedMemoryDependency) int {
		if left.Hash < right.Hash {
			return -1
		}
		if left.Hash > right.Hash {
			return 1
		}
		return 0
	})
	return dependencies
}

func (captured *CapturedEffectiveMemory) Project(ctx context.Context, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	if captured == nil || captured.reader == nil || captured.evidence == nil || captured.generation.Validate() != nil {
		return domain.EffectiveMemoryPage{}, domain.ErrIntegrity
	}
	request, err := normalizeEffectiveMemoryRequest(captured.generation.Graph.RepoID, request)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	if request.Selection != captured.selection || request.Content != captured.content {
		return domain.EffectiveMemoryPage{}, domain.ErrValidation
	}
	queries, err := newFrozenMemoryQueries(captured.reader, captured.evidence, &captured.generation, false)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	page, err := queries.Project(ctx, captured.generation.Graph.RepoID, request)
	if canceled := ctx.Err(); canceled != nil {
		return domain.EffectiveMemoryPage{}, canceled
	}
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return page, nil
}

func (workers *MemoryPreparationWorkers) CaptureEffectiveMemorySource(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource) (*CapturedEffectiveMemory, error) {
	return workers.captureEffectiveMemorySource(ctx, repo, request, previous, nil, nil)
}

func (workers *MemoryPreparationWorkers) captureEffectiveMemorySource(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource, delta *memoryGraphDelta, reference func(context.Context) error) (*CapturedEffectiveMemory, error) {
	request, err := normalizeEffectiveMemoryRequest(repo, request)
	if err != nil {
		return nil, err
	}
	if request.Cursor != "" {
		return nil, domain.ErrValidation
	}
	return memoryPreparationReadForRepo(ctx, workers, repo, func(bound context.Context) (*CapturedEffectiveMemory, error) {
		bound, cancel := context.WithTimeout(bound, 30*time.Second)
		defer cancel()
		_, generation, err := workers.memorySourceGeneration(bound, repo)
		if err != nil {
			return nil, err
		}
		if generation == nil || workers.projectionMemory != nil {
			return nil, domain.ErrCatalogUnsupported
		}
		if previous != nil && previous.checkpoint.Epoch != generation.Graph.Epoch {
			return nil, domain.ErrCatalogResetRequired
		}
		graphStore, graphOK := workers.meta.(outbound.RepositoryGraphSourceStore)
		factory, factoryOK := workers.meta.(outbound.GraphSourceReaderFactory)
		objects, objectsOK := workers.blobs.(outbound.BoundedMemoryReader)
		if !graphOK || !factoryOK || !objectsOK {
			return nil, domain.ErrCatalogUnsupported
		}
		graph, err := captureGraphSource(bound, graphStore, repo, previous, delta)
		if err != nil {
			return nil, err
		}
		if graph.Checkpoint() != generation.Graph {
			return nil, domain.ErrConflict
		}
		metadata, view, entries, err := graph.prepareInputs(bound)
		if err != nil {
			return nil, err
		}
		evidence, err := factory.BuildGraphSourceReader(bound, graph.Checkpoint(), metadata, entries)
		if err != nil {
			return nil, err
		}
		if evidence == nil {
			return nil, domain.ErrIntegrity
		}
		reader := &capturedMemoryReader{repo: repo, view: domain.RepositoryView{Snapshots: view.Snapshots, Refs: view.Refs, History: view.History, Revision: view.Revision}, objects: make(map[domain.ContentHash]json.RawMessage)}
		if request.Selection.Branch != "" {
			pins, supported := workers.meta.(outbound.MemoryCapturePinReader)
			if !supported {
				return nil, domain.ErrCatalogUnsupported
			}
			reader.prPins, reader.gitPins, err = pins.ReadMemoryPinsForCapture(bound, repo)
			if err != nil {
				return nil, err
			}
			if err := validateCapturedMemoryPins(bound, reader); err != nil {
				return nil, err
			}
			reader.prPins, err = cloneMemoryInput(bound, reader.prPins)
			if err != nil {
				return nil, err
			}
			reader.gitPins, err = cloneMemoryInput(bound, reader.gitPins)
			if err != nil {
				return nil, err
			}
			reader.pinsCaptured = true
		}
		recorder := &recordingMemoryReader{capturedMemoryReader: reader, read: objects.GetMemoryForCapture}
		var witnesses map[domain.ContentHash]domain.MemoryObjectWitness
		if witnessed, supported := workers.blobs.(outbound.WitnessedMemoryReader); supported {
			witnesses = make(map[domain.ContentHash]domain.MemoryObjectWitness)
			witnessSet := domain.NewMemoryObjectWitnessSet(repo)
			recorder.read = func(ctx context.Context, repo, hash domain.ContentHash, budget int) (domain.MemoryDigest, error) {
				object, err := witnessed.CaptureMemoryObject(ctx, repo, hash, budget)
				if err != nil {
					return domain.MemoryDigest{}, err
				}
				if object.Witness.Validate() != nil || object.Witness.RepoID != repo || object.Witness.Hash != hash ||
					object.Witness.Available != (object.Digest != nil) {
					return domain.MemoryDigest{}, domain.ErrIntegrity
				}
				if err := witnessSet.Add(object.Witness); err != nil {
					return domain.MemoryDigest{}, err
				}
				witnesses[hash] = object.Witness.Clone()
				if object.Digest == nil {
					return domain.MemoryDigest{}, domain.ErrNotFound
				}
				return *object.Digest, nil
			}
		}
		queries, err := newFrozenMemoryQueries(recorder, evidence, generation, false)
		if err != nil {
			return nil, err
		}
		if _, err := queries.SelectMemory(bound, repo, request.Selection); err != nil {
			return nil, err
		}
		if reference != nil {
			if err := reference(bound); err != nil {
				return nil, err
			}
		}
		_, after, err := workers.memorySourceGeneration(bound, repo)
		if err != nil {
			return nil, err
		}
		if after == nil || *after != *generation {
			return nil, domain.ErrConflict
		}
		if err := bound.Err(); err != nil {
			return nil, err
		}
		return &CapturedEffectiveMemory{generation: *generation, selection: request.Selection, content: request.Content, graph: graph, reader: reader, evidence: evidence, witnesses: witnesses}, nil
	})
}

func validateCapturedMemoryPins(ctx context.Context, reader *capturedMemoryReader) error {
	if len(reader.prPins) > maxProjectionSnapshots || len(reader.gitPins) > maxProjectionSnapshots {
		return domain.ErrMemoryProjectionLimit
	}
	unique := make(map[string]bool)
	for _, pin := range reader.prPins {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pin.Validate() != nil || pin.RepoID != reader.repo || unique[pin.ID] {
			return domain.ErrIntegrity
		}
		unique[pin.ID] = true
	}
	clear(unique)
	for _, pin := range reader.gitPins {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pin.Validate() != nil || pin.RepoID != reader.repo || unique[pin.ID] {
			return domain.ErrIntegrity
		}
		unique[pin.ID] = true
	}
	raw, err := json.Marshal(struct {
		PR  []domain.PRContributionMemory
		Git []domain.GitContributionMemory
	}{reader.prPins, reader.gitPins})
	if err != nil {
		return domain.ErrIntegrity
	}
	if len(raw) > maxPRMemoryMetadataBytes {
		return domain.ErrMemoryProjectionLimit
	}
	return ctx.Err()
}
