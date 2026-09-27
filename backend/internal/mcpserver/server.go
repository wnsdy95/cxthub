// Package mcpserver is the dedicated remote MCP composition root.
package mcpserver

import (
	"fmt"
	"net/http"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// Handler is the independent MCP composition root. Only query ports are
// supplied to the protocol adapter. OAuth persists consent and credentials in
// the shared database; no GitHub, email, ingestion or maintenance worker starts.
func Handler(r *serverruntime.Runtime) (http.Handler, error) {
	changes, ok := r.Store.(outbound.GitChangeStore)
	if !ok {
		return nil, fmt.Errorf("Git evidence query store unavailable")
	}
	scans, err := app.NewGitScanQuery(r.Context)
	if err != nil {
		return nil, err
	}
	s, err := delivery.NewServer(r.Context, r.Identity, r.Store, r.PublicURL)
	if err != nil {
		return nil, err
	}
	s.SetGitChanges(app.NewGitChangeQuery(changes))
	s.SetGitScans(scans)
	s.SetCodeApplicability(r.Context)
	s.SetEffectiveMemory(r.Context)
	return r.HealthHandler(s.Handler()), nil
}
