//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationCheckpointArrayPayloadTraceCountsEveryBody(test *testing.T) {
	trace := &legacyNormalizationParserReusePGTrace{}
	trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: legacyNormalizationBatchObjectInsert,
		Args: []any{"doc", "stage", []string{"first", "second"}, [][]byte{[]byte("abc"), []byte("de")}}})
	trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1", Args: []any{[]byte("f")}})
	if counts := trace.snapshot(); counts.bodyArguments != 3 || counts.bodyBytes != 6 || counts.maxBodyArguments != 2 || counts.maxBodyBytes != 5 || counts.objectInserts != 1 {
		test.Fatal("array payloads disappeared from byte or argument accounting", counts)
	}
	hash := domain.HashContent([]byte("decoded input"))
	repeat := &normalizationParserRepeatPGTrace{input: map[domain.ContentHash]struct{}{hash: {}}}
	repeat.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: legacyNormalizationBatchObjectInsert,
		Args: []any{"doc", "stage", []string{string(hash), "other"}, [][]byte{[]byte("decoded input"), []byte("control")}}})
	if repeat.inputUploads.Load() != 1 || repeat.objects.Load() != 1 {
		test.Fatal("array payload hid a repeated decoded-input upload", repeat.inputUploads.Load(), repeat.objects.Load())
	}
}
