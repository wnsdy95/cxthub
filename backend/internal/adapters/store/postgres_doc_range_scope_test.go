//go:build postgres

package store

import (
	"sync/atomic"
)

type docRangeScopeTracePG struct {
	docRangeGenerationTracer
	isolation  atomic.Int64
	identities atomic.Int64
}

func (trace *docRangeScopeTracePG) facts() [2]int64 {
	return [2]int64{trace.descriptors.Load(), trace.objects.Load()}
}
