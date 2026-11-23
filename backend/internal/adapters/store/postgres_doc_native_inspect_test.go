//go:build postgres

package store

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docNativeInspectPGContext struct {
	context.Context
	inspected atomic.Bool
}

func (ctx *docNativeInspectPGContext) Err() error {
	if !ctx.inspected.Load() {
		var callers [4]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		preparation, _ := frames.Next()
		consumer, _ := frames.Next()
		if strings.HasSuffix(preparation.Function, "domain.(*legacyCanonicalPreparation).step") &&
			strings.HasSuffix(consumer.Function, "domain.(*LegacyCanonicalEncoding).Next") {
			ctx.inspected.Store(true)
		}
	}
	return ctx.Context.Err()
}
