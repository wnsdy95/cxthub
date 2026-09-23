package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.EnterpriseStore = (*FSStore)(nil)

type enterpriseAccount struct {
	Aliases    []string                      `json:"aliases,omitempty"`
	Enterprise domain.Enterprise             `json:"enterprise"`
	Members    []domain.EnterpriseMembership `json:"members"`
	Audit      []domain.EnterpriseAuditEvent `json:"audit"`
}

func (s *FSStore) enterpriseAccountPath(id string) string {
	return filepath.Join(s.dataDir, "enterprise-accounts", id+".json")
}
func (s *FSStore) readEnterpriseAccount(id string) (enterpriseAccount, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return enterpriseAccount{}, err
	}
	var a enterpriseAccount
	if err := readJSON(s.enterpriseAccountPath(id), &a); err != nil {
		return a, err
	}
	if a.Enterprise.ID != id {
		return a, domain.ErrIntegrity
	}
	if err := domain.ValidateEnterprise(a.Enterprise); err != nil {
		return a, storedIdentityIntegrity(err)
	}
	for _, m := range a.Members {
		if m.EnterpriseID != id || domain.ValidateEnterpriseMembership(m) != nil {
			return a, domain.ErrIntegrity
		}
	}
	return a, nil
}
func (s *FSStore) writeEnterpriseAccount(a enterpriseAccount) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return writeAtomic(s.enterpriseAccountPath(a.Enterprise.ID), data)
}
func (s *FSStore) CreateEnterprise(ctx context.Context, e domain.Enterprise, owner domain.EnterpriseMembership) error {
	if err := domain.ValidateEnterprise(e); err != nil {
		return err
	}
	if err := domain.ValidateEnterpriseMembership(owner); err != nil {
		return err
	}
	if owner.EnterpriseID != e.ID || owner.Role != domain.EnterpriseOwner {
		return domain.ErrValidation
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.GetEnterpriseBySlug(ctx, e.Slug); err == nil {
		return domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if _, err := s.GetEnterprise(ctx, e.ID); err == nil {
		return domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return s.writeEnterpriseAccount(enterpriseAccount{Enterprise: e, Members: []domain.EnterpriseMembership{owner}, Audit: []domain.EnterpriseAuditEvent{}})
}
func (s *FSStore) GetEnterprise(_ context.Context, id string) (domain.Enterprise, error) {
	a, err := s.readEnterpriseAccount(id)
	return a.Enterprise, err
}
func (s *FSStore) enterpriseAccounts() ([]enterpriseAccount, error) {
	return readTeamRecords(filepath.Join(s.dataDir, "enterprise-accounts"), func(id string, a enterpriseAccount) error {
		if a.Enterprise.ID != id {
			return domain.ErrIntegrity
		}
		return domain.ValidateEnterprise(a.Enterprise)
	})
}
func (s *FSStore) GetEnterpriseBySlug(_ context.Context, slug string) (domain.Enterprise, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Enterprise{}, domain.ErrValidation
	}
	all, err := s.enterpriseAccounts()
	if err != nil {
		return domain.Enterprise{}, err
	}
	for _, a := range all {
		if a.Enterprise.Slug == slug || slices.Contains(a.Aliases, slug) {
			return a.Enterprise, nil
		}
	}
	return domain.Enterprise{}, domain.ErrNotFound
}
func (s *FSStore) ListEnterprisesForUser(ctx context.Context, user string) ([]domain.Enterprise, error) {
	if err := domain.ValidateExternalID(user); err != nil {
		return nil, err
	}
	all, err := s.enterpriseAccounts()
	if err != nil {
		return nil, err
	}
	out := []domain.Enterprise{}
	for _, a := range all {
		joined := false
		for _, m := range a.Members {
			if m.UserID == user {
				joined = true
				break
			}
		}
		if !joined {
			orgs, err := s.ListEnterpriseOrganizations(ctx, a.Enterprise.ID)
			if err != nil {
				return nil, err
			}
			for _, o := range orgs {
				if _, err := s.GetOrganizationMembership(ctx, o.ID, user); err == nil {
					joined = true
					break
				} else if !errors.Is(err, domain.ErrNotFound) {
					return nil, err
				}
			}
		}
		if joined {
			out = append(out, a.Enterprise)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}
func (s *FSStore) UpdateEnterprise(_ context.Context, e domain.Enterprise) error {
	if err := domain.ValidateEnterprise(e); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	a, err := s.readEnterpriseAccount(e.ID)
	if err != nil {
		return err
	}
	if a.Enterprise.Slug != e.Slug {
		return domain.ErrConflict
	}
	a.Enterprise = e
	return s.writeEnterpriseAccount(a)
}
func (s *FSStore) ListEnterpriseMembers(_ context.Context, id string) ([]domain.EnterpriseMembership, error) {
	a, err := s.readEnterpriseAccount(id)
	return a.Members, err
}
func (s *FSStore) PutEnterpriseMember(_ context.Context, m domain.EnterpriseMembership) error {
	m.User = nil
	if err := domain.ValidateEnterpriseMembership(m); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	a, err := s.readEnterpriseAccount(m.EnterpriseID)
	if err != nil {
		return err
	}
	next := []domain.EnterpriseMembership{}
	for _, old := range a.Members {
		if old.UserID == m.UserID {
			m.CreatedAt = old.CreatedAt
		} else {
			next = append(next, old)
		}
	}
	next = append(next, m)
	hasOwner := false
	for _, member := range next {
		hasOwner = hasOwner || member.Role == domain.EnterpriseOwner
	}
	if !hasOwner {
		return domain.ErrConflict
	}
	a.Members = next
	return s.writeEnterpriseAccount(a)
}
func (s *FSStore) RemoveEnterpriseMember(_ context.Context, id, user string) error {
	if err := domain.ValidateExternalID(user); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	a, err := s.readEnterpriseAccount(id)
	if err != nil {
		return err
	}
	next := []domain.EnterpriseMembership{}
	hasOwner := false
	for _, m := range a.Members {
		if m.UserID != user {
			next = append(next, m)
			hasOwner = hasOwner || m.Role == domain.EnterpriseOwner
		}
	}
	if !hasOwner {
		return domain.ErrConflict
	}
	a.Members = next
	return s.writeEnterpriseAccount(a)
}

type enterpriseOrganization struct {
	OrganizationID string `json:"organization_id"`
	EnterpriseID   string `json:"enterprise_id"`
}

func (s *FSStore) ListEnterpriseOrganizations(ctx context.Context, id string) ([]domain.Organization, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return nil, err
	}
	links, err := readTeamRecords(filepath.Join(s.dataDir, "enterprise-organizations"), func(key string, l enterpriseOrganization) error {
		if key != l.OrganizationID || domain.ValidateOrganizationID(l.OrganizationID) != nil || domain.ValidateEnterpriseID(l.EnterpriseID) != nil {
			return domain.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := []domain.Organization{}
	for _, l := range links {
		if l.EnterpriseID == id {
			o, err := s.GetOrganization(ctx, l.OrganizationID)
			if err != nil {
				return nil, err
			}
			out = append(out, o)
		}
	}
	return out, nil
}
func (s *FSStore) OrganizationEnterprise(ctx context.Context, org string) (domain.Enterprise, error) {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return domain.Enterprise{}, err
	}
	var link enterpriseOrganization
	if err := readJSON(filepath.Join(s.dataDir, "enterprise-organizations", org+".json"), &link); err != nil {
		return domain.Enterprise{}, err
	}
	if link.OrganizationID != org {
		return domain.Enterprise{}, domain.ErrIntegrity
	}
	return s.GetEnterprise(ctx, link.EnterpriseID)
}
func (s *FSStore) SetOrganizationEnterprise(ctx context.Context, org, id string) error {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	path := filepath.Join(s.dataDir, "enterprise-organizations", org+".json")
	if id == "" {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := s.GetEnterprise(ctx, id); err != nil {
		return err
	}
	if _, err := s.GetOrganization(ctx, org); err != nil {
		return err
	}
	if old, err := s.OrganizationEnterprise(ctx, org); err == nil && old.ID != id {
		return domain.ErrConflict
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	data, _ := json.Marshal(enterpriseOrganization{OrganizationID: org, EnterpriseID: id})
	return writeAtomic(path, data)
}
func (s *FSStore) AppendEnterpriseAudit(_ context.Context, e domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateAuditID(e.ID); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	a, err := s.readEnterpriseAccount(e.EnterpriseID)
	if err != nil {
		return err
	}
	a.Audit = append(a.Audit, e)
	return s.writeEnterpriseAccount(a)
}
func (s *FSStore) ListEnterpriseAudit(_ context.Context, id string, limit int) ([]domain.EnterpriseAuditEvent, error) {
	a, err := s.readEnterpriseAccount(id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []domain.EnterpriseAuditEvent{}
	for i := len(a.Audit) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, a.Audit[i])
	}
	return out, nil
}
