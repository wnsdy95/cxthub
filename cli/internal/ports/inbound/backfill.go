package inbound

import (
	"context"
	"time"
)

// HistoricalSync is separate from ordinary push: it never moves refs, completes
// a PR publication or changes a pending pointer. A narrow sync mock or legacy
// store may omit this optional local-worker capability.
type HistoricalSync interface {
	SyncHistorical(context.Context, SyncInput, int) (HistoricalSyncOutput, error)
}

type HistoricalSyncOutput struct {
	Completed   int
	Failed      int
	Pending     int
	Busy        bool
	NextAttempt time.Time
}
