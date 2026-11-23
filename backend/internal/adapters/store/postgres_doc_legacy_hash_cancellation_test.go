//go:build postgres

package store

import (
	"context"
	"runtime"
)

type legacySourceHashCancellationContext struct {
	context.Context
	cancel      context.CancelFunc
	checks      int
	checkpoints [3][2]runtime.Frame
}

func (ctx *legacySourceHashCancellationContext) Err() error {
	if ctx.checks < len(ctx.checkpoints) {
		var callers [8]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for position := range ctx.checkpoints[ctx.checks] {
			frame, _ := frames.Next()
			ctx.checkpoints[ctx.checks][position] = frame
		}
	}
	ctx.checks++
	if ctx.checks == len(ctx.checkpoints) {
		ctx.cancel()
	}
	return ctx.Context.Err()
}
