//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestHistoryDocumentProofErrorPGDiagnostics(test *testing.T) {
	locked := &pgconn.PgError{Code: "55P03", Message: "synthetic-private-row", Detail: "synthetic-private-SQL-body"}
	deadlock := &pgconn.PgError{Code: "40P01", Message: "synthetic deadlock"}
	serialization := &pgconn.PgError{Code: "40001", Message: "synthetic serialization"}
	unrelated := errors.New("synthetic unrelated error")
	for _, selected := range []struct {
		name    string
		input   error
		want    error
		message string
	}{
		{"version_changed", errHistoryDocumentProofVersionChanged, errHistoryDocumentProofVersionChanged, "conflict: history document proof row version changed"},
		{"missing_row", pgx.ErrNoRows, errHistoryDocumentProofRowMissing, "conflict: history document proof row missing"},
		{"wrapped_missing_row", fmt.Errorf("synthetic-private-wrapper: %w", pgx.ErrNoRows), errHistoryDocumentProofRowMissing, "conflict: history document proof row missing"},
		{"not_found", domain.ErrNotFound, errHistoryDocumentProofRowMissing, "conflict: history document proof row missing"},
		{"wrapped_not_found", fmt.Errorf("synthetic-private-wrapper: %w", domain.ErrNotFound), errHistoryDocumentProofRowMissing, "conflict: history document proof row missing"},
		{"nowait_locked", locked, errHistoryDocumentProofRowLocked, "conflict: history document proof row locked"},
		{"wrapped_nowait_locked", fmt.Errorf("synthetic-private-wrapper: %w", locked), errHistoryDocumentProofRowLocked, "conflict: history document proof row locked"},
		{"unclassified_conflict", domain.ErrConflict, domain.ErrConflict, "conflict"},
		{"deadlock", deadlock, deadlock, ""},
		{"serialization", serialization, serialization, ""},
		{"cancellation", context.Canceled, context.Canceled, ""},
		{"unrelated", unrelated, unrelated, ""},
		{"nil", nil, nil, ""},
	} {
		test.Run(selected.name, func(test *testing.T) {
			got := historyDocumentProofErrorPG(selected.input)
			if got != selected.want {
				test.Fatalf("error mapping changed: got %v, want %v", got, selected.want)
			}
			if selected.message == "" {
				return
			}
			if got.Error() != selected.message {
				test.Fatalf("diagnostic must contain only its fixed label: got %q, want %q", got.Error(), selected.message)
			}
			if !errors.Is(got, domain.ErrConflict) || !errors.Is(fmt.Errorf("outer: %w", got), domain.ErrConflict) {
				test.Fatal("diagnostic lost conflict identity through wrapping")
			}
			var databaseError *pgconn.PgError
			if errors.As(got, &databaseError) {
				test.Fatal("diagnostic retained the underlying database error")
			}
		})
	}
}
