package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRMissingOrderingIsPendingConflictNotServerFailure(t *testing.T) {
	code, status := mapError(fmt.Errorf("PR base: %w", domain.ErrPROrderingPending))
	if code != "ordering_evidence_pending" || status != http.StatusConflict {
		t.Fatalf("incorrect pending response: %s %d", code, status)
	}
}
