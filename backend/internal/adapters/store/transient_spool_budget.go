package store

import (
	"math"
	"os"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type transientSpoolConfiguration struct {
	previousLimit int64
	previousPeak  int64
	closing       bool
}

type transientSpoolBudget struct {
	mu            sync.Mutex
	limit         int64
	used          int64
	peak          int64
	files         int
	configuration *transientSpoolConfiguration
}

type transientSpoolAllocation struct {
	owner  *transientSpoolBudget
	bytes  int64
	closed bool
}

var processTransientSpools transientSpoolBudget

func configureTransientSpoolBudget(limit int64) (func() error, error) {
	if limit <= 0 {
		return nil, domain.ErrValidation
	}
	budget := &processTransientSpools
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.configuration != nil || budget.files != 0 || budget.used != 0 {
		return nil, domain.ErrConflict
	}
	configuration := &transientSpoolConfiguration{previousLimit: budget.limit, previousPeak: budget.peak}
	budget.configuration = configuration
	budget.limit, budget.peak = limit, 0
	return func() error {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if budget.configuration != configuration {
			return nil
		}
		configuration.closing = true
		if budget.files != 0 || budget.used != 0 {
			return domain.ErrResourceOverloaded
		}
		budget.restore()
		return nil
	}, nil
}

func (budget *transientSpoolBudget) restore() {
	configuration := budget.configuration
	if configuration != nil && configuration.closing && budget.files == 0 && budget.used == 0 {
		budget.limit, budget.peak = configuration.previousLimit, configuration.previousPeak
		budget.configuration = nil
	}
}

func transientSpoolBudgetUsage() outbound.TransientSpoolBudgetUsage {
	budget := &processTransientSpools
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return outbound.TransientSpoolBudgetUsage{LimitBytes: budget.limit, UsedBytes: budget.used, PeakBytes: budget.peak, OpenFiles: budget.files}
}

func newTransientSpoolAllocation() (*transientSpoolAllocation, error) {
	budget := &processTransientSpools
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.files == int(^uint(0)>>1) || budget.configuration != nil && budget.configuration.closing {
		return nil, domain.ErrResourceOverloaded
	}
	budget.files++
	return &transientSpoolAllocation{owner: budget}, nil
}

func (allocation *transientSpoolAllocation) Reserve(bytes int64) error {
	if allocation == nil || allocation.owner == nil || bytes < 0 {
		return domain.ErrValidation
	}
	budget := allocation.owner
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if allocation.closed {
		return os.ErrClosed
	}
	if bytes > math.MaxInt64-budget.used ||
		bytes > math.MaxInt64-allocation.bytes || budget.limit > 0 && bytes > budget.limit-budget.used {
		return domain.ErrResourceOverloaded
	}
	allocation.bytes += bytes
	budget.used += bytes
	budget.peak = max(budget.peak, budget.used)
	return nil
}

func (allocation *transientSpoolAllocation) Release(bytes int64) {
	if allocation == nil || allocation.owner == nil {
		return
	}
	budget := allocation.owner
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if allocation.closed || bytes < 0 || bytes > allocation.bytes {
		return
	}
	allocation.bytes -= bytes
	budget.used -= bytes
}

func (allocation *transientSpoolAllocation) Close() {
	if allocation == nil || allocation.owner == nil {
		return
	}
	budget := allocation.owner
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if allocation.closed {
		return
	}
	allocation.closed = true
	budget.used -= allocation.bytes
	allocation.bytes = 0
	budget.files--
	budget.restore()
}

func (storage *FSStore) ConfigureTransientSpoolBudget(limit int64) (func() error, error) {
	return configureTransientSpoolBudget(limit)
}

func (storage *FSStore) TransientSpoolBudgetUsage() outbound.TransientSpoolBudgetUsage {
	return transientSpoolBudgetUsage()
}

var _ outbound.TransientSpoolBudgetStore = (*FSStore)(nil)
