//go:build postgres

package store

import (
	"context"
	"runtime"
	"strings"
)

type docNativeEventsPGContext struct {
	context.Context
	loads int
}

func (ctx *docNativeEventsPGContext) Err() error {
	var callers [3]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
	caller, _ := frames.Next()
	consumer, _ := frames.Next()
	if strings.HasSuffix(caller.Function, "domain.readLegacyByteSourcePart") &&
		strings.HasSuffix(consumer.Function, "domain.(*legacyNormalizationInput).part") {
		ctx.loads++
	}
	return ctx.Context.Err()
}
