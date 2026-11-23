//go:build postgres

package http

import (
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

type documentPreparationRevocationStore struct {
	*store.PostgresStore
	before    func() error
	triggered atomic.Bool
	enqueues  atomic.Int32
}

func (storage *documentPreparationRevocationStore) readiness(ready bool, err error) (bool, error) {
	if err == nil && !ready && !storage.triggered.Swap(true) {
		err = storage.before()
	}
	return ready, err
}
