package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHubStore = (*FSStore)(nil)

func (s *FSStore) githubPath(kind, id string) string {
	return filepath.Join(s.dataDir, "github", kind, opaqueName(id)+".json")
}
func (s *FSStore) githubWrite(kind, id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(s.githubPath(kind, id), raw)
}
func githubFSList[T any](s *FSStore, kind string) ([]T, error) {
	out := []T{}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "github", kind))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var v T
		if err := readJSON(filepath.Join(s.dataDir, "github", kind, e.Name()), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *FSStore) GetGitHubConnection(_ context.Context, id string) (v domain.GitHubConnection, err error) {
	err = readJSON(s.githubPath("connections", id), &v)
	return
}
func (s *FSStore) PutGitHubConnection(ctx context.Context, v domain.GitHubConnection) error {
	if domain.ValidateNamespaceID(v.NamespaceID) != nil || v.Installation.ID <= 0 {
		return domain.ErrValidation
	}
	all, err := s.ListGitHubConnections(ctx)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.NamespaceID != v.NamespaceID && c.Installation.ID == v.Installation.ID {
			return domain.ErrConflict
		}
	}
	return s.githubWrite("connections", v.NamespaceID, v)
}
func (s *FSStore) ListGitHubConnections(_ context.Context) ([]domain.GitHubConnection, error) {
	return githubFSList[domain.GitHubConnection](s, "connections")
}
func (s *FSStore) GetGitHubRequest(_ context.Context, id string) (v domain.GitHubConnectRequest, err error) {
	err = readJSON(s.githubPath("requests", id), &v)
	return
}
func (s *FSStore) PutGitHubRequest(ctx context.Context, v domain.GitHubConnectRequest) error {
	return s.githubWrite("requests", v.Hash, v)
}
func (s *FSStore) GetGitHubDelivery(_ context.Context, id string) (v domain.GitHubDelivery, err error) {
	err = readJSON(s.githubPath("deliveries", id), &v)
	return
}
func (s *FSStore) PutGitHubDelivery(ctx context.Context, v domain.GitHubDelivery) error {
	return s.githubWrite("deliveries", v.ID, v)
}
func (s *FSStore) ListGitHubDeliveries(_ context.Context) ([]domain.GitHubDelivery, error) {
	return githubFSList[domain.GitHubDelivery](s, "deliveries")
}
func (s *FSStore) ListGitHubIdentities(_ context.Context) ([]domain.GitHubIdentity, error) {
	return githubFSList[domain.GitHubIdentity](s, "identities")
}
func (s *FSStore) PutGitHubIdentity(ctx context.Context, v domain.GitHubIdentity) error {
	if v.ExternalID <= 0 || domain.ValidateExternalID(v.UserID) != nil {
		return domain.ErrValidation
	}
	all, err := s.ListGitHubIdentities(ctx)
	if err != nil {
		return err
	}
	for _, p := range all {
		if (p.UserID == v.UserID) != (p.ExternalID == v.ExternalID) {
			return domain.ErrConflict
		}
	}
	return s.githubWrite("identities", v.UserID, v)
}
func (s *FSStore) ReplaceGitHubTeamMembers(_ context.Context, ns string, v []domain.GitHubTeamMember) error {
	return s.githubWrite("members", ns, v)
}

// Called within the organization mutation lock. Rejoining must wait for a new
// authoritative synchronization, rather than revive an old imported grant.
func (s *FSStore) removeGitHubOrganizationMember(ctx context.Context, orgID, userID string) error {
	org, err := s.GetOrganization(ctx, orgID)
	if err != nil {
		return err
	}
	var grants []domain.GitHubTeamMember
	if err = readJSON(s.githubPath("members", org.NamespaceID), &grants); errors.Is(err, domain.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	next := []domain.GitHubTeamMember{}
	for _, grant := range grants {
		if grant.UserID != userID {
			next = append(next, grant)
		}
	}
	return s.ReplaceGitHubTeamMembers(ctx, org.NamespaceID, next)
}

func (s *FSStore) ListTeamMembers(ctx context.Context, teamID string) ([]domain.TeamMembership, error) {
	manual, err := s.manualTeamMembers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	team, err := s.GetTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	org, err := s.GetOrganization(ctx, team.OrganizationID)
	if err != nil {
		return nil, err
	}
	c, err := s.GetGitHubConnection(ctx, org.NamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return manual, nil
	}
	if err != nil {
		return nil, err
	}
	if !c.Enabled || c.Status != "connected" {
		return manual, nil
	}
	var grants []domain.GitHubTeamMember
	err = readJSON(s.githubPath("members", org.NamespaceID), &grants)
	if errors.Is(err, domain.ErrNotFound) {
		return manual, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range manual {
		seen[m.UserID] = true
	}
	for _, g := range grants {
		if g.TeamID != teamID || g.NamespaceID != org.NamespaceID || seen[g.UserID] || !g.ExpiresAt.After(time.Now()) {
			continue
		}
		if _, err = s.GetOrganizationMembership(ctx, org.ID, g.UserID); errors.Is(err, domain.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		manual = append(manual, domain.TeamMembership{TeamID: teamID, OrganizationID: org.ID, UserID: g.UserID, Role: domain.TeamMember, CreatedAt: g.ExpiresAt.Add(-10 * time.Minute), Source: "github"})
		seen[g.UserID] = true
	}
	sort.Slice(manual, func(i, j int) bool { return manual[i].UserID < manual[j].UserID })
	return manual, nil
}
