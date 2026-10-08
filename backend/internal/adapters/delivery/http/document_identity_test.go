package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestUnsupportedDocumentIdentityHasExplicitResponse(t *testing.T) {
	for _, err := range []error{domain.ErrUnsupportedDocumentIdentity, fmt.Errorf("document: %w", domain.ErrUnsupportedDocumentIdentity)} {
		code, status := mapError(err)
		if code != "unsupported_document_identity" || status != http.StatusConflict {
			t.Fatal(code, status)
		}
	}
}
