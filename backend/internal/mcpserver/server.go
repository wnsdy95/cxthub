// Package mcpserver is the dedicated remote MCP composition root.
package mcpserver

import (
	"fmt"
	"net/http"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// Handler is the independent MCP composition root. Only the reads of the
// session structure are supplied to the protocol adapter. OAuth persists
// consent and credentials in the shared database; no GitHub, email, ingestion
// or maintenance worker starts.
func Handler(r *serverruntime.Runtime, resourceOrigin string) (http.Handler, error) {
	if r == nil || r.Context != nil || r.RepositoryAssets != nil || r.DocumentPublications != nil || r.Anchored == nil {
		return nil, fmt.Errorf("MCP requires an independently assembled query runtime")
	}
	s, err := delivery.NewServerWithOrigins(r.Anchored, r.Identity, r.Store, r.PublicURL, resourceOrigin)
	if err != nil {
		return nil, err
	}
	if err := s.SetDocumentIdentities(r.DocumentIdentities); err != nil {
		return nil, err
	}
	s.SetAnchoredQueries(r.Anchored)
	return r.HealthHandler(s.Handler()), nil
}
