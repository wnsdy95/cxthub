package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.TeamStore = (*FSStore)(nil)

func (s *FSStore) teamPath(id string) string { return filepath.Join(s.dataDir, "teams", id+".json") }
func (s *FSStore) teamMembersPath(id string) string {
	return filepath.Join(s.dataDir, "team-members", id)
}
func (s *FSStore) teamGrantsPath(id string) string {
	return filepath.Join(s.dataDir, "team-repositories", id)
}
func (s *FSStore) CreateTeam(ctx context.Context, t domain.Team) error {
	if err := domain.ValidateTeam(t); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.GetOrganization(ctx, t.OrganizationID); err != nil {
		return err
	}
	all, err := s.ListTeams(ctx, t.OrganizationID)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID == t.ID || other.Slug == t.Slug {
			return domain.ErrConflict
		}
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(s.teamPath(t.ID), data)
}
func (s *FSStore) GetTeam(_ context.Context, id string) (domain.Team, error) {
	if err := domain.ValidateTeamID(id); err != nil {
		return domain.Team{}, err
	}
	var t domain.Team
	if err := readJSON(s.teamPath(id), &t); err != nil {
		return t, err
	}
	if t.ID != id {
		return t, domain.ErrIntegrity
	}
	if err := domain.ValidateTeam(t); err != nil {
		return t, storedIdentityIntegrity(err)
	}
	return t, nil
}
func readTeamRecords[T any](dir string, validate func(string, T) error) ([]T, error) {
	out := []T{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var value T
		if err := readJSON(filepath.Join(dir, entry.Name()), &value); err != nil {
			return nil, err
		}
		if err := validate(strings.TrimSuffix(entry.Name(), ".json"), value); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, value)
	}
	return out, nil
}
func (s *FSStore) ListTeams(_ context.Context, org string) ([]domain.Team, error) {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return nil, err
	}
	all, err := readTeamRecords(filepath.Join(s.dataDir, "teams"), func(id string, t domain.Team) error {
		if t.ID != id {
			return domain.ErrIntegrity
		}
		return domain.ValidateTeam(t)
	})
	if err != nil {
		return nil, err
	}
	out := []domain.Team{}
	for _, t := range all {
		if t.OrganizationID == org {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}
func (s *FSStore) DeleteTeam(_ context.Context, id string) error {
	if err := domain.ValidateTeamID(id); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	// Remove the authority record first. Interrupted cleanup cannot grant access.
	if err := os.Remove(s.teamPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(s.teamMembersPath(id)); err != nil {
		return err
	}
	return os.RemoveAll(s.teamGrantsPath(id))
}
func (s *FSStore) PutTeamMember(ctx context.Context, m domain.TeamMembership) error {
	if err := domain.ValidateTeamMembership(m); err != nil {
		return err
	}
	t, err := s.GetTeam(ctx, m.TeamID)
	if err != nil {
		return err
	}
	if t.OrganizationID != m.OrganizationID {
		return domain.ErrForbidden
	}
	if _, err = s.GetOrganizationMembership(ctx, m.OrganizationID, m.UserID); err != nil {
		return err
	}
	members, err := s.ListTeamMembers(ctx, m.TeamID)
	if err != nil {
		return err
	}
	for _, old := range members {
		if old.UserID == m.UserID {
			m.CreatedAt = old.CreatedAt
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.teamMembersPath(m.TeamID), opaqueName(m.UserID)+".json"), data)
}
func (s *FSStore) RemoveTeamMember(_ context.Context, team, user string) error {
	if err := domain.ValidateTeamID(team); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(user); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.teamMembersPath(team), opaqueName(user)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *FSStore) ListTeamMembers(_ context.Context, team string) ([]domain.TeamMembership, error) {
	if err := domain.ValidateTeamID(team); err != nil {
		return nil, err
	}
	return readTeamRecords(s.teamMembersPath(team), func(key string, m domain.TeamMembership) error {
		if m.TeamID != team || key != opaqueName(m.UserID) {
			return domain.ErrIntegrity
		}
		return domain.ValidateTeamMembership(m)
	})
}
func (s *FSStore) PutTeamRepositoryGrant(ctx context.Context, g domain.TeamRepositoryGrant) error {
	if err := domain.ValidateTeamRepositoryGrant(g); err != nil {
		return err
	}
	team, err := s.GetTeam(ctx, g.TeamID)
	if err != nil {
		return err
	}
	if team.OrganizationID != g.OrganizationID {
		return domain.ErrForbidden
	}
	org, err := s.GetOrganization(ctx, g.OrganizationID)
	if err != nil {
		return err
	}
	repository, err := s.GetRepository(ctx, g.RepositoryID)
	if err != nil {
		return err
	}
	if repository.OwnerNamespaceID != org.NamespaceID {
		return domain.ErrForbidden
	}
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.teamGrantsPath(g.TeamID), g.RepositoryID+".json"), data)
}
func (s *FSStore) RemoveTeamRepositoryGrant(_ context.Context, team, repository string) error {
	if err := domain.ValidateTeamID(team); err != nil {
		return err
	}
	if err := domain.ValidateRepositoryID(repository); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.teamGrantsPath(team), repository+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *FSStore) ListTeamRepositoryGrants(_ context.Context, team string) ([]domain.TeamRepositoryGrant, error) {
	if err := domain.ValidateTeamID(team); err != nil {
		return nil, err
	}
	return readTeamRecords(s.teamGrantsPath(team), func(key string, g domain.TeamRepositoryGrant) error {
		if g.TeamID != team || key != g.RepositoryID {
			return domain.ErrIntegrity
		}
		return domain.ValidateTeamRepositoryGrant(g)
	})
}
func (s *FSStore) RepositoryTeamAccess(ctx context.Context, repository, user string) ([]domain.TeamRepositoryAccess, error) {
	if err := domain.ValidateRepositoryID(repository); err != nil {
		return nil, err
	}
	if err := domain.ValidateExternalID(user); err != nil {
		return nil, err
	}
	out := []domain.TeamRepositoryAccess{}
	r, err := s.GetRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	if r.OwnerNamespaceID == "" {
		return out, nil
	}
	ns, err := s.GetNamespace(ctx, r.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if ns.Kind != domain.NamespaceOrganization {
		return out, nil
	}
	if _, err := s.GetOrganizationMembership(ctx, ns.OrganizationID, user); errors.Is(err, domain.ErrNotFound) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	teams, err := s.ListTeams(ctx, ns.OrganizationID)
	if err != nil {
		return nil, err
	}
	for _, team := range teams {
		members, err := s.ListTeamMembers(ctx, team.ID)
		if err != nil {
			return nil, err
		}
		member := false
		for _, m := range members {
			if m.UserID == user && m.OrganizationID == ns.OrganizationID {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		grants, err := s.ListTeamRepositoryGrants(ctx, team.ID)
		if err != nil {
			return nil, err
		}
		for _, g := range grants {
			if g.RepositoryID == repository && g.OrganizationID == ns.OrganizationID {
				out = append(out, domain.TeamRepositoryAccess{TeamID: team.ID, RepositoryID: repository, OrganizationNamespaceID: ns.ID, UserID: user, OrganizationMember: true, TeamMember: true, Role: g.Role})
			}
		}
	}
	return out, nil
}
