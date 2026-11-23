//go:build postgres

package store

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

type docCaptureWorkPGSpan struct{ from, bytes int }

type docCaptureCommitTraceKey struct{}

type docCaptureCommitHook struct{ run func() }

type docCaptureWorkPGTrace struct {
	bodyReads      atomic.Int64
	queries        atomic.Int64
	mu             sync.Mutex
	spans          []docCaptureWorkPGSpan
	commit         atomic.Pointer[docCaptureCommitHook]
	captureWitness atomic.Bool
}

func (trace *docCaptureWorkPGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	if strings.HasPrefix(query.SQL, "SELECT source.doc_range_source_version,incarnation.incarnation::text,octet_length(source.bytes)") {
		trace.captureWitness.Store(true)
	}
	if strings.Contains(strings.ReplaceAll(query.SQL, "octet_length(source.bytes)", ""), "source.bytes") {
		trace.bodyReads.Add(1)
		if len(query.Args) == 4 {
			from, fromOK := query.Args[2].(int)
			length, lengthOK := query.Args[3].(int)
			if fromOK && lengthOK {
				trace.mu.Lock()
				trace.spans = append(trace.spans, docCaptureWorkPGSpan{from: from, bytes: length})
				trace.mu.Unlock()
			}
		}
	}
	if hook := trace.commit.Load(); hook != nil && strings.EqualFold(strings.TrimSpace(query.SQL), "commit") {
		return context.WithValue(ctx, docCaptureCommitTraceKey{}, hook)
	}
	return ctx
}

func (*docCaptureWorkPGTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hook, ok := ctx.Value(docCaptureCommitTraceKey{}).(*docCaptureCommitHook); ok {
		hook.run()
	}
}

func TestDocCaptureWorkPayloadTraceDistinguishesMetadataAndWholeReads(test *testing.T) {
	trace := &docCaptureWorkPGTrace{}
	spoolTrace := &docSpoolWorkPGTrace{}
	for ordinal, query := range []string{
		`SELECT octet_length(source.bytes),source.doc_range_source_version`,
		`SELECT substring(source.bytes FROM $3 FOR $4),octet_length(source.bytes)`,
		`SELECT source.bytes`,
	} {
		trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: query})
		spoolTrace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: query})
		if trace.bodyReads.Load() != int64(ordinal) || spoolTrace.sourceReads.Load() != int64(ordinal) {
			test.Fatal("payload measurement hid a whole source read or counted metadata as body", ordinal, trace.bodyReads.Load(), spoolTrace.sourceReads.Load())
		}
	}
}

type docCaptureConfirmationContext struct {
	context.Context
	paused  atomic.Bool
	release sync.Once
	entered chan struct{}
	resume  chan struct{}
	target  string
}

func (ctx *docCaptureConfirmationContext) Err() error {
	if !ctx.paused.Load() {
		var callers [2]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		caller, _ := frames.Next()
		target := ctx.target
		if target == "" {
			target = "store.(*preparedLegacyCaptureWork).handoffCanonical"
		}
		if strings.HasSuffix(caller.Function, target) && ctx.paused.CompareAndSwap(false, true) {
			close(ctx.entered)
			<-ctx.resume
		}
	}
	return ctx.Context.Err()
}

func (ctx *docCaptureConfirmationContext) unblock() {
	ctx.release.Do(func() { close(ctx.resume) })
}
