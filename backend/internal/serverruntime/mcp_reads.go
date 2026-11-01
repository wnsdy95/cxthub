package serverruntime

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (runtime *Runtime) configureMCPReads(profile documentquery.Profile, repositories []domain.ContentHash, rootPublication bool) error {
	contextProfile := inbound.ContextQueryCompatibility
	if _, pinned := runtime.Store.(outbound.RepositoryTransactions); pinned {
		contextProfile = inbound.ContextQueryPinnedSnapshots
	}
	fields, _ := runtime.Store.(outbound.DocFindFieldsStore)
	display, _ := runtime.Store.(outbound.DocDisplayReader)
	identities := app.ReadDocumentIdentities(runtime.Store, runtime.Store)
	queries, err := app.NewReadServices(app.ReadServicesConfig{
		Metadata: runtime.Store, Blobs: runtime.Store,
		DocumentProfile: profile, ContextProfile: contextProfile,
		SupportedDocumentIdentities: identities,
		RootPublicationEnabled:      rootPublication, PreparedMemoryRepositories: repositories,
		DocumentFindFields: fields, DocumentDisplayReads: display,
	})
	if err != nil {
		return fmt.Errorf("configure MCP read services: %w", err)
	}
	runtime.DocumentEvents = queries.Documents
	runtime.DocumentQueries = queries.Documents
	runtime.DocumentQueryProfile = queries.DocumentProfile
	runtime.DocumentIdentities = append([]domain.DocumentIdentity(nil), identities...)
	runtime.ContextQueries = queries.Context
	runtime.ContextQueryProfile = queries.ContextProfile
	runtime.PreparedMemory = queries.PreparedMemory
	runtime.MemorySnapshots = queries.MemorySnapshots
	runtime.MemoryLive = queries.MemoryLive
	runtime.Archive = queries.Archive
	runtime.GitChanges = queries.GitChanges
	runtime.GitScans = queries.GitScans
	runtime.CodeApplicability = queries.CodeApplicability
	runtime.ConversationFind = queries.ConversationFind
	return nil
}
