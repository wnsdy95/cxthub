// Package auth provides outbound.AuthProvider adapter (team token).
//
// v1 (demo): Simple team model. If token exists, extract team from token string; otherwise, allow "default" team (convenience for local/single-team demo). RepoVisible is always true (v1: team boundary enforcement).
// impl phase: Enhance with Postgres team_tokens query + team-specific visibility enforcement.
package auth

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// TeamTokenAuth is a simple AuthProvider implementation based on team tokens (v1 model).
type TeamTokenAuth struct{}

// NewTeamTokenAuth creates a TeamTokenAuth.
func NewTeamTokenAuth() *TeamTokenAuth { return &TeamTokenAuth{} }

var _ outbound.AuthProvider = (*TeamTokenAuth)(nil)

// ResolveTeam interprets bearer team token into team identifier.
// Format "cxt_team_<team>" yields <team>, otherwise non-empty token itself, empty string yields "default".
func (a *TeamTokenAuth) ResolveTeam(_ context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "default", nil
	}
	if t, ok := strings.CutPrefix(token, "cxt_team_"); ok && t != "" {
		return t, nil
	}
	return token, nil
}

// RepoVisible determines if team has access to repo (v1: always allowed).
func (a *TeamTokenAuth) RepoVisible(_ context.Context, _ string, _ domain.ContentHash) (bool, error) {
	return true, nil
}
