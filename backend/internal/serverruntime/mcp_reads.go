package serverruntime

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// configureMCPReads gives the tool server what it reads: the session
// structure, and nothing that writes.
func (runtime *Runtime) configureMCPReads() error {
	accounts, _ := runtime.Store.(outbound.RepositoryStore)
	anchored, err := app.NewAnchoredReadService(runtime.Store, runtime.Store, accounts)
	if err != nil {
		return fmt.Errorf("configure MCP session reads: %w", err)
	}
	runtime.Anchored = anchored
	runtime.DocumentIdentities = append([]domain.DocumentIdentity(nil), app.ReadDocumentIdentities(runtime.Store, runtime.Store)...)
	return nil
}
