//go:build postgres

package app

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docRangeNativeBlockPagedPGContext struct {
	context.Context
	entered atomic.Bool
	decode  bool
}

func (ctx *docRangeNativeBlockPagedPGContext) Err() error {
	if !ctx.entered.Load() {
		var callers [12]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if ctx.decode && (strings.HasSuffix(frame.Function, "domain.(*legacyNativeBlockList).step") ||
				strings.HasSuffix(frame.Function, "domain.(*legacyNativeBlockList).advance")) ||
				!ctx.decode && strings.HasSuffix(frame.Function, "domain.(*legacyCanonicalPreparation).step") {
				ctx.entered.Store(true)
				break
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}
