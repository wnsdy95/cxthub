//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryDependencyReadSnapshot(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	writer, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer writer.Close()
	snapshot := collaborationSnapshot(test, storage, repo, "memory-dependency-snapshot")
	memory, err := service.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: snapshot, Summary: "same transaction"})
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: memory, CodeCommit: effectiveOID(1)}}
	err = storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before, err := service.QueryEffectiveMemory(bound, repo, request)
		if err != nil {
			return err
		}
		generation, err := storage.ReadRepositoryMemorySourceGeneration(bound, repo)
		if err != nil {
			return err
		}
		if _, err := writer.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(memory)); err != nil {
			return err
		}
		current, err := storage.ReadRepositoryMemorySourceGeneration(bound, repo)
		if err != nil {
			return err
		}
		held, err := service.QueryEffectiveMemory(bound, repo, request)
		if err != nil {
			return err
		}
		if current != generation || !reflect.DeepEqual(before, held) {
			test.Fatal("memory metadata and ownership crossed read snapshots")
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryEffectiveMemory(ctx, repo, request); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("later query ignored committed ownership revocation", err)
	}
}
