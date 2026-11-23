//go:build postgres

package store

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
)

type metadataSpoolCancellation struct {
	context.Context
	directory string
	prefix    string
	cancel    context.CancelFunc
	flushed   atomic.Int64
}

func (observed *metadataSpoolCancellation) Err() error {
	if err := observed.Context.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(observed.directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		prefix := observed.prefix
		if prefix == "" {
			prefix = "cxthub-doc-range-"
		}
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 0 && observed.flushed.CompareAndSwap(0, info.Size()) {
			observed.cancel()
		}
	}
	return observed.Context.Err()
}
