package app

import (
	"context"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (captured *CapturedEffectiveMemory) ObjectWitnesses(ctx context.Context) ([]domain.MemoryObjectWitness, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if captured == nil || captured.reader == nil || captured.generation.Validate() != nil || captured.generation.Graph.RepoID != captured.reader.repo {
		return nil, domain.ErrIntegrity
	}
	if captured.witnesses == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if len(captured.witnesses) != len(captured.reader.objects) {
		return nil, domain.ErrIntegrity
	}
	witnesses := make([]domain.MemoryObjectWitness, 0, len(captured.witnesses))
	witnessSet := domain.NewMemoryObjectWitnessSet(captured.reader.repo)
	for hash, raw := range captured.reader.objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		witness, present := captured.witnesses[hash]
		if !present || witness.RepoID != captured.reader.repo || witness.Hash != hash || witness.Available != (raw != nil) || witness.Validate() != nil {
			return nil, domain.ErrIntegrity
		}
		witnesses = append(witnesses, witness.Clone())
		if err := witnessSet.Add(witness); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(witnesses, func(left, right domain.MemoryObjectWitness) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	return witnesses, nil
}

func (workers *MemoryPreparationWorkers) ValidateCapturedMemorySource(ctx context.Context, repo domain.ContentHash, captured *CapturedEffectiveMemory) error {
	if domain.ValidateContentHash(repo) != nil {
		return domain.ErrValidation
	}
	witnesses, err := captured.ObjectWitnesses(ctx)
	if err != nil {
		return err
	}
	if captured.generation.Graph.RepoID != repo {
		return domain.ErrIntegrity
	}
	reader, supported := workers.blobs.(outbound.WitnessedMemoryReader)
	if !supported || workers.projectionMemory != nil {
		return domain.ErrCatalogUnsupported
	}
	transactions, known := workers.meta.(outbound.RepositoryTransactionPresence)
	if !known {
		return domain.ErrCatalogUnsupported
	}
	if transactions.InTransaction(ctx) {
		return domain.ErrConflict
	}
	_, err = memoryPreparationReadForRepo(ctx, workers, repo, func(bound context.Context) (struct{}, error) {
		_, generation, err := workers.memorySourceGeneration(bound, repo)
		if err != nil {
			return struct{}{}, err
		}
		if generation == nil {
			return struct{}{}, domain.ErrCatalogUnsupported
		}
		if *generation != captured.generation {
			return struct{}{}, domain.ErrConflict
		}
		return struct{}{}, reader.ValidateMemoryObjectWitnesses(bound, repo, witnesses)
	})
	return err
}
