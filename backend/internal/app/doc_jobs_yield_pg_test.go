//go:build postgres

package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGPreparedRootDoesNotYieldBorrowedTransaction(t *testing.T) {
	for _, mode := range []string{"read", "write", "foreign-read"} {
		t.Run(mode, func(t *testing.T) {
			service, storage, repo := collaborationPG(t)
			transactionStore := storage
			if mode == "foreign-read" {
				_, transactionStore, _ = collaborationPG(t)
			}
			ctx, gate := admissionTestContext(time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			bound, permit, release := ownedYieldPermit(t, ctx)
			defer release()
			var uploaded atomic.Bool
			uploadDone := make(chan error, 1)
			go func() {
				uploadDone <- resourceWork(ctx, ResourceBulk, func(context.Context) error { uploaded.Store(true); return nil })
			}()
			waitResourceQueue(t, gate, ResourceBulk, 1)
			publication := &yieldingRootPublication{resources: outbound.RootPublicationResources{HeapBytes: 4096}}
			within := func(transaction context.Context) error {
				if !storage.InTransaction(transaction) {
					t.Fatal("test did not borrow PostgreSQL transaction")
				}
				if err := service.yieldRootPreparation(transaction, publication); err != nil {
					return err
				}
				gate.mu.Lock()
				preserved := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked && len(gate.waiters[ResourceBulk]) == 1
				gate.mu.Unlock()
				if !preserved || uploaded.Load() {
					t.Error("publication yielded its borrowed transaction")
				}
				return nil
			}
			var err error
			if mode == "write" {
				err = transactionStore.WithinRepository(bound, repo, within)
			} else {
				err = transactionStore.WithinReadSnapshot(bound, within)
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			if err := <-uploadDone; err != nil {
				t.Fatal(err)
			}
			assertResourcesReleased(t, gate)
		})
	}
}
