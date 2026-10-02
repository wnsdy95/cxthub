package backendclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// VerifyEmptyRepository uses the existing context query, without a branch,
// position or pagination filter. Decode history and required revision fields
// explicitly: an older/incomplete response must never become an empty proof.
func (c *BackendClient) VerifyEmptyRepository(ctx context.Context, repo string) (domain.EmptyRepositoryProof, error) {
	var proof domain.EmptyRepositoryProof
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return proof, err
	}
	var wire struct {
		Version   int                `json:"version"`
		StateHash domain.ContentHash `json:"state_hash"`
		Position  domain.ContentHash `json:"position"`
		Branch    string             `json:"branch"`
		Snapshots []json.RawMessage  `json:"snapshots"`
		History   []json.RawMessage  `json:"history"`
		Segments  json.RawMessage    `json:"segments"`
		Inclusion json.RawMessage    `json:"inclusion"`
		Revision  *struct {
			Graph    *uint64 `json:"graph,string"`
			Pending  *uint64 `json:"pending,string"`
			Evidence uint64  `json:"evidence,string"`
		} `json:"revision"`
	}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo)+"/context-query?scope=all", nil, &wire, 64<<20); err != nil {
		return proof, err // Includes 403, 404 and transport errors; no fallback.
	}
	if wire.Version != domain.QueryContractVersion || domain.ValidateContentHash(wire.StateHash) != nil || wire.Revision == nil || wire.Revision.Graph == nil || wire.Revision.Pending == nil || wire.Snapshots == nil || wire.History == nil || wire.Position != "" || wire.Branch != "" || (len(wire.Segments) > 0 && string(wire.Segments) != "null") || (len(wire.Inclusion) > 0 && string(wire.Inclusion) != "null") {
		return proof, domain.ErrHashMismatch
	}
	if len(wire.Snapshots) != 0 || len(wire.History) != 0 {
		return proof, fmt.Errorf("%w: %w", domain.ErrAgentContextUnavailable, domain.ErrRepositoryHasContext)
	}
	return domain.EmptyRepositoryProof{RepositoryID: repo, StateHash: wire.StateHash, Revision: domain.RepositoryRevision{Graph: *wire.Revision.Graph, Pending: *wire.Revision.Pending, Evidence: wire.Revision.Evidence}}, nil
}
