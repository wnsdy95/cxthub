package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) gitAssociationStatusPage(ctx context.Context, repo domain.Repo, args toolArgs) (string, error) {
	query, supported := server.context.(inbound.GitAssociationStatusQuery)
	if !supported {
		return "", domain.ErrGitAssociationStatusUnavailable
	}
	if args.Kind == "" {
		args.Kind = "integration"
	}
	if args.State == "" {
		args.State = "pending"
	}
	if args.Limit == 0 {
		args.Limit = 10
	}
	if args.Limit < 1 || args.Limit > 20 {
		return "", domain.ErrValidation
	}
	cursor, err := cursorFor(string(repo.ID), "context_link_status", args)
	if err != nil {
		return "", err
	}
	selection := domain.GitAssociationStatusRequest{Kind: args.Kind, State: args.State, Limit: args.Limit, Cursor: cursor.After}
	if err := selection.Validate(); err != nil {
		return "", err
	}
	page, err := query.GetGitAssociationStatus(ctx, repo.ID, selection)
	if err != nil {
		return "", err
	}
	if page.RepoID != repo.ID || page.Kind != args.Kind || page.Filter != args.State || len(page.Items) > selection.Limit {
		return "", domain.ErrIntegrity
	}
	for {
		output := page
		if page.NextCursor != "" {
			cursor.After = page.NextCursor
			output.NextCursor = encodeCursor(cursor)
		}
		var encoded strings.Builder
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		err := encoder.Encode(output)
		raw := strings.TrimSuffix(encoded.String(), "\n")
		if err != nil || len(raw) <= pageBytes {
			return raw, err
		}
		if len(page.Items) <= 1 {
			return "", fmt.Errorf("context link status exceeds response budget")
		}
		page.Items = page.Items[:len(page.Items)-1]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
}
