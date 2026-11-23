//go:build postgres

package store

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

type legacyBytePrepareBoundaryContext struct {
	context.Context
	paused          atomic.Bool
	release         sync.Once
	entered, resume chan struct{}
}

func (ctx *legacyBytePrepareBoundaryContext) Err() error {
	if !ctx.paused.Load() {
		var callers [4]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		hash, _ := frames.Next()
		verifier, _ := frames.Next()
		if strings.HasSuffix(hash.Function, "domain.HashContentContext") &&
			strings.HasSuffix(verifier.Function, "store.(*PostgresStore).verifyLegacyMonolithicRangeSource") &&
			ctx.paused.CompareAndSwap(false, true) {
			close(ctx.entered)
			<-ctx.resume
		}
	}
	return ctx.Context.Err()
}

func (ctx *legacyBytePrepareBoundaryContext) unblock() {
	ctx.release.Do(func() { close(ctx.resume) })
}
