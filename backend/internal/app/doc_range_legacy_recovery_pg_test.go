//go:build postgres

package app

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
)

type docRangeLegacyRecoveryPGVerification struct {
	context.Context
	preparation atomic.Bool
	step        atomic.Bool
	event       atomic.Bool
}

func (ctx *docRangeLegacyRecoveryPGVerification) Err() error {
	if !ctx.preparation.Load() || !ctx.step.Load() || !ctx.event.Load() {
		var callers [32]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			switch {
			case strings.HasSuffix(frame.Function, "domain.NewLegacyByteSourcePreparation"):
				ctx.preparation.Store(true)
			case strings.HasSuffix(frame.Function, "domain.(*LegacyByteSourcePreparation).Step"):
				ctx.step.Store(true)
			case strings.HasSuffix(frame.Function, "domain.verifyCanonicalEvent"):
				ctx.event.Store(true)
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}
