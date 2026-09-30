package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Diagnostics are opt-in and process-local. They never alter the caller's
// deadline, retry policy, or acknowledged publication progress.
func beginPushDiagnostics(ctx context.Context) (context.Context, func()) {
	return withPushDiagnostics(ctx, os.Stderr, os.Getenv("CXT_SYNC_DIAGNOSTICS") == "1")
}

func withPushDiagnostics(ctx context.Context, output io.Writer, enabled bool) (context.Context, func()) {
	if !enabled || outbound.SyncDiagnosticsFromContext(ctx) != nil {
		return ctx, func() {}
	}
	collector := outbound.NewSyncDiagnostics(64)
	ctx = outbound.WithSyncDiagnostics(ctx, collector)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			if !collector.HasFailures() {
				return
			}
			// The typed report has no body, URL, header, identity, path, raw error
			// or cancellation-cause fields. Formatting/output cannot fail a push.
			if raw, err := json.Marshal(collector.Snapshot()); err == nil {
				_, _ = fmt.Fprintf(output, "cxt sync diagnostics: %s\n", raw)
			}
		})
	}
}
