//go:build postgres

package store

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docCanonicalPagesPGContext struct {
	context.Context
	native           atomic.Bool
	inspectionChecks atomic.Int64
}

func (ctx *docCanonicalPagesPGContext) Err() error {
	var callers [4]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
	caller, _ := frames.Next()
	consumer, _ := frames.Next()
	if strings.HasSuffix(caller.Function, "domain.newLegacyCanonicalEncodingNative") ||
		strings.HasSuffix(caller.Function, "domain.finishLegacyCanonicalEncodingNative") {
		ctx.native.Store(true)
	}
	if strings.HasSuffix(caller.Function, "domain.(*legacyCanonicalPreparation).step") &&
		strings.HasSuffix(consumer.Function, "domain.(*LegacyCanonicalEncoding).Next") {
		ctx.inspectionChecks.Add(1)
	}
	return ctx.Context.Err()
}
