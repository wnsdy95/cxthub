//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGMemoryReadAdmitsBeforeOpeningSnapshot(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	snapshot := collaborationSnapshot(test, storage, repo, "memory admission")
	previous, err := service.PutMemoryDigestCAS(systemTestContext(), repo, domain.MemoryDigest{SnapshotID: snapshot, Summary: "before queue"})
	if err != nil {
		test.Fatal(err)
	}
	observed := &graphReadPGTransactions{PostgresStore: storage}
	service.meta = observed
	ctx, gate := admissionTestContext(5 * time.Second)
	ctx, cancel := context.WithTimeout(inbound.WithSystemActor(ctx), 10*time.Second)
	defer cancel()
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	type result struct {
		digest domain.MemoryDigest
		err    error
	}
	done := make(chan result, 1)
	go func() { digest, err := service.GetMemoryDigest(ctx, repo, snapshot); done <- result{digest, err} }()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	if observed.active.Load() != 0 || observed.reads.Load() != 0 {
		test.Fatal("memory queue retains PostgreSQL read snapshot")
	}
	if _, err := service.PutMemoryDigestCAS(systemTestContext(), repo, domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: previous, Summary: "committed while queued"}); err != nil {
		test.Fatal(err)
	}
	release()
	got := <-done
	if got.err != nil || got.digest.Summary != "committed while queued" {
		test.Fatal("admitted reader opened stale snapshot", got.err, got.digest.Summary)
	}
	if observed.active.Load() != 0 || observed.reads.Load() != 1 {
		test.Fatal("owned read snapshot leaked or duplicated")
	}
	assertResourcesReleased(test, gate)
}

func TestPGMemoryReadBorrowedTransactionDoesNotWait(test *testing.T) {
	for _, mode := range []string{"read", "write"} {
		test.Run(mode, func(test *testing.T) {
			service, storage, repo := collaborationPG(test)
			snapshot := collaborationSnapshot(test, storage, repo, "borrowed memory")
			if _, err := service.PutMemoryDigestCAS(systemTestContext(), repo, domain.MemoryDigest{SnapshotID: snapshot, Summary: "current memory"}); err != nil {
				test.Fatal(err)
			}
			ctx, gate := admissionTestContext(10 * time.Second)
			ctx, cancel := context.WithTimeout(inbound.WithSystemActor(ctx), 5*time.Second)
			defer cancel()
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			body := func(bound context.Context) error {
				_, err := service.GetMemoryDigest(bound, repo, snapshot)
				if !errors.Is(err, domain.ErrResourceOverloaded) {
					test.Fatalf("borrowed %s read admitted without capacity: %v", mode, err)
				}
				return nil
			}
			if mode == "read" {
				err = storage.WithinReadSnapshot(ctx, body)
			} else {
				err = storage.WithinRepository(ctx, repo, body)
			}
			if err != nil {
				test.Fatal(err)
			}
			release()
			assertResourcesReleased(test, gate)
			bound, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			body = func(transaction context.Context) error {
				digest, err := service.GetMemoryDigest(transaction, repo, snapshot)
				if err == nil && digest.Summary != "current memory" {
					test.Fatal("borrowed admitted read changed memory")
				}
				return err
			}
			if mode == "read" {
				err = storage.WithinReadSnapshot(bound, body)
			} else {
				err = storage.WithinRepository(bound, repo, body)
			}
			if err != nil {
				test.Fatal(err)
			}
			release()
			assertResourcesReleased(test, gate)
		})
	}
}
