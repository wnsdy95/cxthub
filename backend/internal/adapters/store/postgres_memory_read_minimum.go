//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MinimumMemoryReadStore = (*PostgresStore)(nil)

func (storage *PostgresStore) withMinimumMemoryReadSnapshot(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error), read func(context.Context, pgx.Tx, domain.ContentHash) error) error {
	if authorize == nil {
		return domain.ErrValidation
	}
	identifier, err := domain.MemoryReadTargetID(ctx, target)
	if err != nil {
		return err
	}
	if _, borrowed := ctx.Value(repositoryTxKey{}).(*repositoryTx); borrowed {
		return domain.ErrConflict
	}
	transaction, _, err := storage.memoryReadSnapshot(ctx, target.RepoID, identifier)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	snapshot := &repositoryTx{Tx: transaction, owner: storage, readOnly: true, repo: target.RepoID}
	ctx, err = authorize(context.WithValue(ctx, repositoryTxKey{}, snapshot))
	if err != nil {
		return err
	}
	if ctx == nil || ctx.Value(repositoryTxKey{}) != snapshot || snapshot.Tx != transaction ||
		snapshot.owner != storage || !snapshot.readOnly || snapshot.repo != target.RepoID {
		return domain.ErrConflict
	}
	var enabled bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_read_work_scopes WHERE repo_id=$1 AND enabled)`, string(target.RepoID)).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return domain.ErrCatalogUnsupported
	}
	if err := read(ctx, transaction, identifier); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) ObserveMemoryReadRequirement(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error) {
	var requirement domain.MemoryReadRequirement
	err := storage.withMinimumMemoryReadSnapshot(ctx, target, authorize, func(bound context.Context, transaction pgx.Tx, identifier domain.ContentHash) error {
		var present bool
		if err := transaction.QueryRow(bound, `SELECT EXISTS(SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2)`, string(target.RepoID), string(target.Selection.SnapshotID)).Scan(&present); err != nil {
			return err
		}
		if !present {
			return domain.ErrNotFound
		}
		source, err := storage.ReadRepositoryMemorySourceGeneration(bound, target.RepoID)
		if err != nil {
			return err
		}
		requirement = domain.MemoryReadRequirement{Version: domain.MemoryReadRequirementVersion, TargetID: identifier, Minimum: source}
		return requirement.Validate(target)
	})
	if err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	return requirement, nil
}

func (storage *PostgresStore) WithMinimumMemoryReadRange(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement, generationID domain.ContentHash, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadStatus, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	if consume == nil {
		return domain.ErrValidation
	}
	if err := domain.ValidateOptionalContentHash(generationID); err != nil {
		return err
	}
	return storage.withMinimumMemoryReadSnapshot(ctx, target, authorize, func(bound context.Context, transaction pgx.Tx, identifier domain.ContentHash) error {
		if err := requirement.Validate(target); err != nil {
			return err
		}
		pinned := generationID != ""
		unavailable := domain.ErrMemoryReadNotReady
		if !pinned {
			work, err := scanMemoryReadWork(bound, transaction.QueryRow(bound, `SELECT `+memoryReadWorkColumns+`
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(identifier)), target.RepoID, identifier)
			if errors.Is(err, domain.ErrNotFound) {
				return unavailable
			}
			if err != nil {
				return err
			}
			if work.State == "rejected" {
				unavailable = domain.ErrMemoryReadNeedsAttention
			}
			generationID = work.QualificationID
			if generationID == "" {
				return unavailable
			}
		}
		_, qualification, err := readMemoryQualification(bound, transaction, target.RepoID, generationID)
		if err != nil {
			return minimumMemoryReadUnavailable(err, pinned, unavailable)
		}
		header, verifier, err := readMemoryReadDescriptor(bound, transaction, target.RepoID, qualification.ReadID)
		if err != nil {
			return minimumMemoryReadUnavailable(err, pinned, unavailable)
		}
		if err := domain.ValidateMemoryReadQualification(bound, qualification, header); err != nil {
			return err
		}
		if header.Source.Graph.RepoID != target.RepoID || header.Rules != target.Rules || header.View.Selection != target.Selection || header.View.Content != target.Content {
			return domain.ErrIntegrity
		}
		if err := requireMemoryReadComparison(bound, transaction, qualification.ReadID, identifier, header, false); err != nil {
			if errors.Is(err, domain.ErrMemoryReadComparisonMismatch) {
				return domain.ErrMemoryReadNeedsAttention
			}
			return err
		}
		current, err := storage.ReadRepositoryMemorySourceGeneration(bound, target.RepoID)
		if err != nil {
			return err
		}
		if err := domain.CheckMemorySourceMinimum(current, qualification.Source); err != nil {
			return err
		}
		if err := domain.CheckMemorySourceMinimum(qualification.Source, requirement.Minimum); err != nil {
			if errors.Is(err, domain.ErrMemoryReadNotReady) {
				return unavailable
			}
			return err
		}
		if err := validateMinimumMemoryObjectWitnesses(bound, transaction, target.RepoID, qualification.Witnesses); err != nil {
			return minimumMemoryReadUnavailable(err, pinned, unavailable)
		}
		status := domain.MemoryReadStatus{Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, Generation: generationID,
			Minimum: requirement.Minimum, Observed: current, Applied: qualification.Source, Complete: true, Freshness: "catching_up"}
		if current == qualification.Source {
			status.Freshness = "current"
		}
		if err := status.Validate(); err != nil {
			return err
		}
		if err := consumeMemoryReadRange(bound, transaction, target.RepoID, qualification.ReadID, header, verifier,
			func(readContext context.Context, header domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				return consume(readContext, status, header, reader)
			}); err != nil {
			if errors.Is(err, domain.ErrMemoryCaptureExpired) && !pinned {
				return unavailable
			}
			return err
		}
		return nil
	})
}

func minimumMemoryReadUnavailable(err error, pinned bool, unavailable error) error {
	if pinned && (errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrMemoryCaptureExpired)) {
		return domain.ErrMemoryCaptureExpired
	}
	if memoryReadWorkUnavailable(err) {
		return unavailable
	}
	return err
}
