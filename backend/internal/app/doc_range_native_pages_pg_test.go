//go:build postgres

package app

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docRangeNativePagesPGContext struct {
	context.Context
	inspected atomic.Bool
	rawDecode bool
}

func (ctx *docRangeNativePagesPGContext) Err() error {
	if !ctx.inspected.Load() {
		var callers [12]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		if ctx.rawDecode {
			for {
				frame, more := frames.Next()
				if strings.HasSuffix(frame.Function, "domain.(*legacyNativeEventList).step") ||
					strings.HasSuffix(frame.Function, "domain.(*legacyNativeEventList).advance") {
					ctx.inspected.Store(true)
					break
				}
				if !more {
					break
				}
			}
			return ctx.Context.Err()
		}
		preparation, _ := frames.Next()
		encoding, _ := frames.Next()
		consumer, _ := frames.Next()
		if strings.HasSuffix(preparation.Function, "domain.(*legacyCanonicalPreparation).step") &&
			strings.HasSuffix(encoding.Function, "domain.(*LegacyCanonicalEncoding).Next") &&
			strings.HasSuffix(consumer.Function, "domain.(*LegacyNormalizationPreparation).stage") {
			ctx.inspected.Store(true)
		}
	}
	return ctx.Context.Err()
}
