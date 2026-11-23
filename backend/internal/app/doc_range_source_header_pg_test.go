//go:build postgres

package app

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docRangeSourceHeaderPGContext struct {
	context.Context
	observe bool
	entered atomic.Bool
}

func (ctx *docRangeSourceHeaderPGContext) Err() error {
	if ctx.observe && !ctx.entered.Load() {
		var callers [20]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, "domain.(*legacySourceEnvelope).step") {
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
