// Package mcpserver is the dedicated remote MCP composition root.
package mcpserver

import (
	"fmt"
	"net/http"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// Handler is the independent MCP composition root. Only query ports are
// supplied to the protocol adapter. OAuth persists consent and credentials in
// the shared database; no GitHub, email, ingestion or maintenance worker starts.
func Handler(r *serverruntime.Runtime, resourceOrigin string) (http.Handler, error) {
	return HandlerWithMemoryPreparation(r, resourceOrigin, nil)
}

func HandlerWithMemoryPreparation(r *serverruntime.Runtime, resourceOrigin string, requester outbound.MemoryPreparationRequester) (http.Handler, error) {
	return HandlerWithReadPreparation(r, resourceOrigin, requester, nil)
}

func HandlerWithReadPreparation(r *serverruntime.Runtime, resourceOrigin string, requester outbound.MemoryPreparationRequester, conversation outbound.ConversationFindPreparationRequester) (http.Handler, error) {
	if r == nil || r.Context != nil || r.RepositoryAssets != nil || r.DocumentPublications != nil || r.RepositoryRefs != nil || r.SnapshotGrafts != nil || r.MemoryDigests != nil || r.HistoryCommands != nil || r.PRPromotions != nil || r.ContextBranches != nil || r.Archive == nil || r.GitChanges == nil || r.GitScans == nil || r.CodeApplicability == nil || r.ConversationFind == nil {
		return nil, fmt.Errorf("MCP requires an independently assembled query runtime")
	}
	s, err := delivery.NewServerWithOrigins(r.Archive, r.Identity, r.Store, r.PublicURL, resourceOrigin)
	if err != nil {
		return nil, err
	}
	if err := s.SetDocumentIdentities(r.DocumentIdentities); err != nil {
		return nil, err
	}
	if err := s.SetContextQuery(r.ContextQueries, r.ContextQueryProfile); err != nil {
		return nil, err
	}
	if err := s.SetDocumentEventQueries(r.DocumentQueries, r.DocumentQueryProfile); err != nil {
		return nil, err
	}
	s.SetGitChanges(r.GitChanges)
	s.SetGitScans(r.GitScans)
	s.SetCodeApplicability(r.CodeApplicability)
	if err := s.SetLiveMemoryQueries(r.MemoryLive); err != nil {
		return nil, err
	}
	if err := s.SetPreparedMemory(r.PreparedMemory); err != nil {
		return nil, err
	}
	if err := s.SetMemorySnapshotQueries(r.MemorySnapshots); err != nil {
		return nil, err
	}
	s.SetMemoryPreparationRequester(requester)
	s.SetConversationFind(r.ConversationFind, conversation)
	return r.HealthHandler(s.Handler()), nil
}
