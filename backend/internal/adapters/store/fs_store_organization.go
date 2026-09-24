package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.OrganizationStore = (*FSStore)(nil)

var fsOrganizationMutationLocks sync.Map

func (s *FSStore) organizationMutationLock() *sync.Mutex {
	lock, _ := fsOrganizationMutationLocks.LoadOrStore(s.dataDir, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) namespacesByIDDir() string { return filepath.Join(s.dataDir, "namespaces", "by-id") }
func (s *FSStore) namespacesBySlugDir() string {
	return filepath.Join(s.dataDir, "namespaces", "by-slug")
}
func (s *FSStore) namespaceAliasesDir() string {
	return filepath.Join(s.dataDir, "namespaces", "aliases")
}
func (s *FSStore) organizationsDir() string { return filepath.Join(s.dataDir, "organizations") }
func (s *FSStore) organizationMembersDir() string {
	return filepath.Join(s.dataDir, "organization-members")
}
func (s *FSStore) organizationPoliciesDir() string {
	return filepath.Join(s.dataDir, "organization-policies")
}
func (s *FSStore) organizationAuditDir() string {
	return filepath.Join(s.dataDir, "organization-audit")
}
func (s *FSStore) breakGlassDir() string { return filepath.Join(s.dataDir, "organization-break-glass") }

func (s *FSStore) CreateNamespace(_ context.Context, ns domain.Namespace) error {
	if err := domain.ValidateNamespaceRecord(ns); err != nil {
		return err
	}
	var alias struct {
		NamespaceID string `json:"namespace_id"`
	}
	if readJSON(filepath.Join(s.namespaceAliasesDir(), ns.Slug+".json"), &alias) == nil && alias.NamespaceID != ns.ID {
		return domain.ErrConflict
	}
	data, _ := json.Marshal(ns)
	slugPath := filepath.Join(s.namespacesBySlugDir(), ns.Slug+".json")
	if err := os.MkdirAll(filepath.Dir(slugPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(slugPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			var existing domain.Namespace
			if readJSON(slugPath, &existing) == nil && existing.ID == ns.ID {
				return nil
			}
			return domain.ErrConflict
		}
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(slugPath)
		return err
	}
	if err := writeAtomic(filepath.Join(s.namespacesByIDDir(), ns.ID+".json"), data); err != nil {
		_ = os.Remove(slugPath)
		return err
	}
	return nil
}

func (s *FSStore) GetNamespace(_ context.Context, id string) (domain.Namespace, error) {
	if err := domain.ValidateNamespaceID(id); err != nil {
		return domain.Namespace{}, err
	}
	var ns domain.Namespace
	err := readJSON(filepath.Join(s.namespacesByIDDir(), id+".json"), &ns)
	if err == nil {
		if ns.ID != id || domain.ValidateNamespaceRecord(ns) != nil {
			return domain.Namespace{}, domain.ErrIntegrity
		}
	}
	return ns, err
}

func (s *FSStore) GetNamespaceBySlug(ctx context.Context, slug string) (domain.Namespace, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Namespace{}, domain.ErrValidation
	}
	var ns domain.Namespace
	err := readJSON(filepath.Join(s.namespacesBySlugDir(), slug+".json"), &ns)
	if err == domain.ErrNotFound {
		var alias struct {
			NamespaceID string `json:"namespace_id"`
		}
		if aliasErr := readJSON(filepath.Join(s.namespaceAliasesDir(), slug+".json"), &alias); aliasErr == nil {
			return s.GetNamespace(ctx, alias.NamespaceID)
		}
	}
	if err == nil && domain.ValidateNamespaceRecord(ns) != nil {
		return domain.Namespace{}, domain.ErrIntegrity
	}
	return ns, err
}

func (s *FSStore) RenameNamespace(ctx context.Context, id, nextSlug string) error {
	if !domain.ValidNamespaceSlug(nextSlug) {
		return domain.ErrValidation
	}
	current, err := s.GetNamespace(ctx, id)
	if err != nil {
		return err
	}
	if current.Slug == nextSlug {
		return nil
	}
	if claimed, claimErr := s.GetNamespaceBySlug(ctx, nextSlug); claimErr == nil && claimed.ID != id {
		return domain.ErrConflict
	}
	next := current
	next.Slug = nextSlug
	_ = os.Remove(filepath.Join(s.namespaceAliasesDir(), nextSlug+".json"))
	if err := s.CreateNamespace(ctx, next); err != nil {
		return err
	}
	alias, _ := json.Marshal(struct {
		NamespaceID string `json:"namespace_id"`
	}{NamespaceID: id})
	if err := writeAtomic(filepath.Join(s.namespaceAliasesDir(), current.Slug+".json"), alias); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.namespacesBySlugDir(), current.Slug+".json"))
	return nil
}

func (s *FSStore) CreateOrganization(
	ctx context.Context,
	organizationRecord domain.Organization,
	ns domain.Namespace,
	owner domain.OrganizationMembership,
	policy domain.OrganizationPolicy,
	audit domain.OrganizationAuditEvent,
) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil || ns.OrganizationID != organizationRecord.ID || ns.ID != organizationRecord.NamespaceID || ns.Slug != organizationRecord.Slug {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationMembershipRecord(owner); err != nil || owner.OrganizationID != organizationRecord.ID || owner.Role != domain.OrganizationOwner {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationPolicy(policy); err != nil || policy.OrganizationID != organizationRecord.ID {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationAuditEvent(audit); err != nil || audit.OrganizationID != organizationRecord.ID {
		return domain.ErrValidation
	}
	if err := s.CreateNamespace(ctx, ns); err != nil {
		return err
	}
	data, _ := json.Marshal(organizationRecord)
	if err := writeAtomic(filepath.Join(s.organizationsDir(), organizationRecord.ID+".json"), data); err != nil {
		s.rollbackOrganizationBootstrap(organizationRecord, ns, owner, policy, audit)
		return err
	}
	if err := s.AddOrganizationMember(ctx, owner); err != nil {
		s.rollbackOrganizationBootstrap(organizationRecord, ns, owner, policy, audit)
		return err
	}
	if err := s.PutOrganizationPolicy(ctx, policy); err != nil {
		s.rollbackOrganizationBootstrap(organizationRecord, ns, owner, policy, audit)
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, audit); err != nil {
		s.rollbackOrganizationBootstrap(organizationRecord, ns, owner, policy, audit)
		return err
	}
	return nil
}

func (s *FSStore) rollbackOrganizationBootstrap(organizationRecord domain.Organization, ns domain.Namespace, owner domain.OrganizationMembership, policy domain.OrganizationPolicy, audit domain.OrganizationAuditEvent) {
	_ = os.Remove(filepath.Join(s.organizationAuditDir(), organizationRecord.ID, audit.CreatedAt.UTC().Format("20060102T150405.000000000Z")+"-"+audit.ID+".json"))
	_ = os.Remove(filepath.Join(s.organizationPoliciesDir(), policy.OrganizationID+".json"))
	_ = os.Remove(filepath.Join(s.organizationMembersDir(), owner.OrganizationID, opaqueName(owner.UserID)+".json"))
	_ = os.Remove(filepath.Join(s.organizationsDir(), organizationRecord.ID+".json"))
	_ = os.Remove(filepath.Join(s.namespacesByIDDir(), ns.ID+".json"))
	_ = os.Remove(filepath.Join(s.namespacesBySlugDir(), ns.Slug+".json"))
}

func (s *FSStore) GetOrganization(_ context.Context, id string) (domain.Organization, error) {
	if err := domain.ValidateOrganizationID(id); err != nil {
		return domain.Organization{}, err
	}
	var organizationRecord domain.Organization
	err := readJSON(filepath.Join(s.organizationsDir(), id+".json"), &organizationRecord)
	if err == nil && (organizationRecord.ID != id || domain.ValidateOrganizationRecord(organizationRecord) != nil) {
		return domain.Organization{}, domain.ErrIntegrity
	}
	return organizationRecord, err
}

func (s *FSStore) UpdateOrganization(_ context.Context, organizationRecord domain.Organization) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	var current domain.Organization
	if err := readJSON(filepath.Join(s.organizationsDir(), organizationRecord.ID+".json"), &current); err != nil {
		return err
	}
	// Identity and namespace ownership are immutable through this operation.
	// Namespace renames require alias-aware migration and are deliberately a
	// separate use case.
	if current.NamespaceID != organizationRecord.NamespaceID || current.Slug != organizationRecord.Slug || current.CreatedBy != organizationRecord.CreatedBy || !current.CreatedAt.Equal(organizationRecord.CreatedAt) {
		return domain.ErrConflict
	}
	data, _ := json.Marshal(organizationRecord)
	return writeAtomic(filepath.Join(s.organizationsDir(), organizationRecord.ID+".json"), data)
}

func (s *FSStore) UpdateOrganizationWithAudit(ctx context.Context, organizationRecord domain.Organization, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, organizationRecord.ID, "organization.profile.updated", "organization", organizationRecord.ID); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetOrganization(ctx, organizationRecord.ID)
	if err != nil {
		return err
	}
	if err := s.UpdateOrganization(ctx, organizationRecord); err != nil {
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		if rollbackErr := s.UpdateOrganization(ctx, previous); rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetOrganizationBySlug(ctx context.Context, slug string) (domain.Organization, error) {
	ns, err := s.GetNamespaceBySlug(ctx, slug)
	if err != nil || ns.Kind != domain.NamespaceOrganization {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.Organization{}, err
	}
	return s.GetOrganization(ctx, ns.OrganizationID)
}

func (s *FSStore) ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.organizationMembersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Organization{}, nil
		}
		return nil, err
	}
	var out []domain.Organization
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := s.organizationMembership(entry.Name(), userID); err != nil {
			continue
		}
		if organizationRecord, err := s.GetOrganization(ctx, entry.Name()); err == nil {
			out = append(out, organizationRecord)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *FSStore) AddOrganizationMember(ctx context.Context, m domain.OrganizationMembership) error {
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	return s.addOrganizationMemberUnlocked(ctx, m)
}

func (s *FSStore) addOrganizationMemberUnlocked(_ context.Context, m domain.OrganizationMembership) error {
	if err := domain.ValidateOrganizationMembershipRecord(m); err != nil {
		return err
	}
	previous, err := s.organizationMembership(m.OrganizationID, m.UserID)
	if err == nil {
		if previous.Role == domain.OrganizationOwner && m.Role != domain.OrganizationOwner {
			if err := s.requireAnotherOrganizationOwner(m.OrganizationID, m.UserID); err != nil {
				return err
			}
		}
		m.CreatedAt = previous.CreatedAt
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	m.User = nil
	data, _ := json.Marshal(m)
	return writeAtomic(filepath.Join(s.organizationMembersDir(), m.OrganizationID, opaqueName(m.UserID)+".json"), data)
}

func (s *FSStore) AddOrganizationMemberWithAudit(ctx context.Context, member domain.OrganizationMembership, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationMembershipRecord(member); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, member.OrganizationID, "organization.member.updated", "user", member.UserID); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, previousErr := s.GetOrganizationMembership(ctx, member.OrganizationID, member.UserID)
	if previousErr != nil && !errors.Is(previousErr, domain.ErrNotFound) {
		return previousErr
	}
	if err := s.addOrganizationMemberUnlocked(ctx, member); err != nil {
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		var rollbackErr error
		if previousErr == nil {
			rollbackErr = s.addOrganizationMemberUnlocked(ctx, previous)
		} else {
			rollbackErr = s.removeOrganizationMemberUnlocked(ctx, member.OrganizationID, member.UserID)
		}
		if rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) organizationMembership(organizationID, userID string) (domain.OrganizationMembership, error) {
	var m domain.OrganizationMembership
	err := readJSON(filepath.Join(s.organizationMembersDir(), organizationID, opaqueName(userID)+".json"), &m)
	if err == nil && domain.ValidateOrganizationMembershipRecord(m) != nil {
		return m, domain.ErrIntegrity
	}
	return m, err
}

func (s *FSStore) requireAnotherOrganizationOwner(organizationID, excludingUserID string) error {
	entries, err := os.ReadDir(filepath.Join(s.organizationMembersDir(), organizationID))
	if err != nil {
		if os.IsNotExist(err) {
			return domain.ErrConflict
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var member domain.OrganizationMembership
		if err := readJSON(filepath.Join(s.organizationMembersDir(), organizationID, entry.Name()), &member); err != nil {
			return err
		}
		if err := domain.ValidateOrganizationMembershipRecord(member); err != nil || member.OrganizationID != organizationID {
			return domain.ErrIntegrity
		}
		if member.UserID != excludingUserID && member.Role == domain.OrganizationOwner {
			return nil
		}
	}
	return domain.ErrConflict
}

func (s *FSStore) GetOrganizationMembership(_ context.Context, organizationID, userID string) (domain.OrganizationMembership, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	return s.organizationMembership(organizationID, userID)
}

func (s *FSStore) RemoveOrganizationMember(ctx context.Context, organizationID, userID string) error {
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	return s.removeOrganizationMemberUnlocked(ctx, organizationID, userID)
}

func (s *FSStore) removeOrganizationMemberUnlocked(ctx context.Context, organizationID, userID string) error {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	previous, previousErr := s.organizationMembership(organizationID, userID)
	if previousErr != nil && !errors.Is(previousErr, domain.ErrNotFound) {
		return previousErr
	}
	if previousErr == nil && previous.Role == domain.OrganizationOwner {
		if err := s.requireAnotherOrganizationOwner(organizationID, userID); err != nil {
			return err
		}
	}
	// Match the PostgreSQL membership FK cascade. Rejoining an organization
	// must not silently reactivate memberships in its former teams.
	teams, err := s.ListTeams(ctx, organizationID)
	if err != nil {
		return err
	}
	for _, team := range teams {
		if err := s.RemoveTeamMember(ctx, team.ID, userID); err != nil {
			return err
		}
	}
	if err := s.removeGitHubOrganizationMember(ctx, organizationID, userID); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(s.organizationMembersDir(), organizationID, opaqueName(userID)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *FSStore) RemoveOrganizationMemberWithAudit(ctx context.Context, organizationID, userID string, event domain.OrganizationAuditEvent) error {
	if err := validateOrganizationMutationAudit(event, organizationID, "organization.member.removed", "user", userID); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetOrganizationMembership(ctx, organizationID, userID)
	if err != nil {
		return err
	}
	teams, err := s.ListTeams(ctx, organizationID)
	if err != nil {
		return err
	}
	var previousTeams []domain.TeamMembership
	for _, team := range teams {
		members, err := s.manualTeamMembers(ctx, team.ID)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.UserID == userID {
				previousTeams = append(previousTeams, member)
			}
		}
	}
	restore := func() error {
		if err := s.addOrganizationMemberUnlocked(ctx, previous); err != nil {
			return domain.ErrIntegrity
		}
		for _, member := range previousTeams {
			if err := s.PutTeamMember(ctx, member); err != nil {
				return domain.ErrIntegrity
			}
		}
		return nil
	}
	if err := s.removeOrganizationMemberUnlocked(ctx, organizationID, userID); err != nil {
		if rollbackErr := restore(); rollbackErr != nil {
			return rollbackErr
		}
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		if rollbackErr := restore(); rollbackErr != nil {
			return rollbackErr
		}
		return err
	}
	return nil
}

func (s *FSStore) ListOrganizationMembers(ctx context.Context, organizationID string) ([]domain.OrganizationMembership, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.organizationMembersDir(), organizationID))
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.OrganizationMembership{}, nil
		}
		return nil, err
	}
	var out []domain.OrganizationMembership
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var m domain.OrganizationMembership
		if readJSON(filepath.Join(s.organizationMembersDir(), organizationID, entry.Name()), &m) != nil {
			continue
		}
		if domain.ValidateOrganizationMembershipRecord(m) != nil {
			return nil, domain.ErrIntegrity
		}
		if u, err := s.GetUser(ctx, m.UserID); err == nil {
			m.User = &u
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *FSStore) PutOrganizationPolicy(_ context.Context, p domain.OrganizationPolicy) error {
	if err := domain.ValidateOrganizationPolicy(p); err != nil {
		return err
	}
	data, _ := json.Marshal(p)
	return writeAtomic(filepath.Join(s.organizationPoliciesDir(), p.OrganizationID+".json"), data)
}

func (s *FSStore) PutOrganizationPolicyWithAudit(ctx context.Context, policy domain.OrganizationPolicy, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationPolicy(policy); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, policy.OrganizationID, "organization.policy.updated", "organization", policy.OrganizationID); err != nil {
		return err
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetOrganizationPolicy(ctx, policy.OrganizationID)
	if err != nil {
		return err
	}
	if err := s.PutOrganizationPolicy(ctx, policy); err != nil {
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		if rollbackErr := s.PutOrganizationPolicy(ctx, previous); rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetOrganizationPolicy(_ context.Context, organizationID string) (domain.OrganizationPolicy, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.OrganizationPolicy{}, err
	}
	var p domain.OrganizationPolicy
	err := readJSON(filepath.Join(s.organizationPoliciesDir(), organizationID+".json"), &p)
	if err == nil && domain.ValidateOrganizationPolicy(p) != nil {
		return p, domain.ErrIntegrity
	}
	return p, err
}

func (s *FSStore) ListRepositoriesForNamespace(_ context.Context, namespaceID string) ([]domain.Repository, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.repositoriesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Repository{}, nil
		}
		return nil, err
	}
	var out []domain.Repository
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var repositoryRecord domain.Repository
		if readJSON(filepath.Join(s.repositoriesDir(), entry.Name()), &repositoryRecord) == nil && repositoryRecord.OwnerNamespaceID == namespaceID {
			if domain.ValidateRepositoryRecord(repositoryRecord) != nil {
				return nil, domain.ErrIntegrity
			}
			out = append(out, repositoryRecord)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *FSStore) CreateOrganizationRepositoryWithAudit(ctx context.Context, repository domain.Repository, owner domain.Membership, event domain.OrganizationAuditEvent) error {
	if err := validateOrganizationRepositoryMutation(repository, owner, event); err != nil {
		return err
	}
	namespace, err := s.GetNamespace(ctx, repository.OwnerNamespaceID)
	if err != nil || namespace.Kind != domain.NamespaceOrganization || namespace.OrganizationID != event.OrganizationID {
		return domain.ErrValidation
	}
	lock := s.organizationMutationLock()
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.GetRepository(ctx, repository.ID); !errors.Is(err, domain.ErrNotFound) {
		if err == nil {
			return domain.ErrConflict
		}
		return err
	}
	if err := s.CreateRepository(ctx, repository); err != nil {
		return err
	}
	if err := s.AddMember(ctx, owner); err != nil {
		if rollbackErr := os.Remove(filepath.Join(s.repositoriesDir(), repository.ID+".json")); rollbackErr != nil && !errors.Is(rollbackErr, os.ErrNotExist) {
			return domain.ErrIntegrity
		}
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		memberErr := s.RemoveMember(ctx, owner.RepositoryID, owner.UserID)
		repositoryErr := os.Remove(filepath.Join(s.repositoriesDir(), repository.ID+".json"))
		if memberErr != nil || repositoryErr != nil && !errors.Is(repositoryErr, os.ErrNotExist) {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) AppendOrganizationAudit(_ context.Context, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil {
		return err
	}
	data, _ := json.Marshal(event)
	name := event.CreatedAt.UTC().Format("20060102T150405.000000000Z") + "-" + event.ID + ".json"
	return writeAtomic(filepath.Join(s.organizationAuditDir(), event.OrganizationID, name), data)
}

func (s *FSStore) ListOrganizationAudit(ctx context.Context, organizationID string, limit int) ([]domain.OrganizationAuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.OrganizationAuditBefore(ctx, domain.AuditCursor{OrganizationID: organizationID}, limit)
}

func (s *FSStore) CreateBreakGlassGrant(_ context.Context, grant domain.BreakGlassGrant) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	data, _ := json.Marshal(grant)
	return writeAtomic(filepath.Join(s.breakGlassDir(), grant.OrganizationID, grant.RepositoryID, opaqueName(grant.UserID), grant.ID+".json"), data)
}

func (s *FSStore) CreateBreakGlassGrantWithAudit(ctx context.Context, grant domain.BreakGlassGrant, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil || event.OrganizationID != grant.OrganizationID || event.ActorID != grant.UserID || event.Action != "organization.break_glass.created" {
		return domain.ErrValidation
	}
	if err := s.CreateBreakGlassGrant(ctx, grant); err != nil {
		return err
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		grantPath := filepath.Join(s.breakGlassDir(), grant.OrganizationID, grant.RepositoryID, opaqueName(grant.UserID), grant.ID+".json")
		if rollbackErr := os.Remove(grantPath); rollbackErr != nil && !os.IsNotExist(rollbackErr) {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetActiveBreakGlassGrant(_ context.Context, organizationID, repositoryID, userID string, now time.Time) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	entries, err := os.ReadDir(filepath.Join(s.breakGlassDir(), organizationID, repositoryID, opaqueName(userID)))
	if err != nil {
		return domain.BreakGlassGrant{}, domain.ErrNotFound
	}
	var latest domain.BreakGlassGrant
	for _, entry := range entries {
		var grant domain.BreakGlassGrant
		if readJSON(filepath.Join(s.breakGlassDir(), organizationID, repositoryID, opaqueName(userID), entry.Name()), &grant) != nil {
			continue
		}
		if grant.ExpiresAt.After(now) && (latest.ID == "" || grant.ExpiresAt.After(latest.ExpiresAt)) {
			latest = grant
		}
	}
	if latest.ID == "" {
		return domain.BreakGlassGrant{}, domain.ErrNotFound
	}
	return latest, nil
}

func (s *FSStore) UseActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time, event domain.OrganizationAuditEvent) (domain.BreakGlassGrant, error) {
	grant, err := s.GetActiveBreakGlassGrant(ctx, organizationID, repositoryID, userID, now)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	event.Reason = grant.Reason
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil || event.OrganizationID != organizationID || event.ActorID != userID || event.Action != "organization.break_glass.used" || event.TargetID != repositoryID {
		return domain.BreakGlassGrant{}, domain.ErrValidation
	}
	if err := s.AppendOrganizationAudit(ctx, event); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}
