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

var _ outbound.EnterpriseStore = (*FSStore)(nil)

var fsEnterpriseMutationLocks sync.Map

func (s *FSStore) enterpriseMutationLock() *sync.Mutex {
	lock, _ := fsEnterpriseMutationLocks.LoadOrStore(s.dataDir, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FSStore) namespacesByIDDir() string { return filepath.Join(s.dataDir, "namespaces", "by-id") }
func (s *FSStore) namespacesBySlugDir() string {
	return filepath.Join(s.dataDir, "namespaces", "by-slug")
}
func (s *FSStore) namespaceAliasesDir() string {
	return filepath.Join(s.dataDir, "namespaces", "aliases")
}
func (s *FSStore) enterprisesDir() string { return filepath.Join(s.dataDir, "enterprises") }
func (s *FSStore) enterpriseMembersDir() string {
	return filepath.Join(s.dataDir, "enterprise-members")
}
func (s *FSStore) enterprisePoliciesDir() string {
	return filepath.Join(s.dataDir, "enterprise-policies")
}
func (s *FSStore) enterpriseAuditDir() string { return filepath.Join(s.dataDir, "enterprise-audit") }
func (s *FSStore) breakGlassDir() string      { return filepath.Join(s.dataDir, "enterprise-break-glass") }

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

func (s *FSStore) CreateEnterprise(
	ctx context.Context,
	ent domain.Enterprise,
	ns domain.Namespace,
	owner domain.EnterpriseMembership,
	policy domain.EnterprisePolicy,
	audit domain.EnterpriseAuditEvent,
) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil || ns.EnterpriseID != ent.ID || ns.ID != ent.NamespaceID || ns.Slug != ent.Slug {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterpriseMembershipRecord(owner); err != nil || owner.EnterpriseID != ent.ID || owner.Role != domain.EnterpriseOwner {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterprisePolicy(policy); err != nil || policy.EnterpriseID != ent.ID {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterpriseAuditEvent(audit); err != nil || audit.EnterpriseID != ent.ID {
		return domain.ErrValidation
	}
	if err := s.CreateNamespace(ctx, ns); err != nil {
		return err
	}
	data, _ := json.Marshal(ent)
	if err := writeAtomic(filepath.Join(s.enterprisesDir(), ent.ID+".json"), data); err != nil {
		s.rollbackEnterpriseBootstrap(ent, ns, owner, policy, audit)
		return err
	}
	if err := s.AddEnterpriseMember(ctx, owner); err != nil {
		s.rollbackEnterpriseBootstrap(ent, ns, owner, policy, audit)
		return err
	}
	if err := s.PutEnterprisePolicy(ctx, policy); err != nil {
		s.rollbackEnterpriseBootstrap(ent, ns, owner, policy, audit)
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, audit); err != nil {
		s.rollbackEnterpriseBootstrap(ent, ns, owner, policy, audit)
		return err
	}
	return nil
}

func (s *FSStore) rollbackEnterpriseBootstrap(ent domain.Enterprise, ns domain.Namespace, owner domain.EnterpriseMembership, policy domain.EnterprisePolicy, audit domain.EnterpriseAuditEvent) {
	_ = os.Remove(filepath.Join(s.enterpriseAuditDir(), ent.ID, audit.CreatedAt.UTC().Format("20060102T150405.000000000Z")+"-"+audit.ID+".json"))
	_ = os.Remove(filepath.Join(s.enterprisePoliciesDir(), policy.EnterpriseID+".json"))
	_ = os.Remove(filepath.Join(s.enterpriseMembersDir(), owner.EnterpriseID, opaqueName(owner.UserID)+".json"))
	_ = os.Remove(filepath.Join(s.enterprisesDir(), ent.ID+".json"))
	_ = os.Remove(filepath.Join(s.namespacesByIDDir(), ns.ID+".json"))
	_ = os.Remove(filepath.Join(s.namespacesBySlugDir(), ns.Slug+".json"))
}

func (s *FSStore) GetEnterprise(_ context.Context, id string) (domain.Enterprise, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return domain.Enterprise{}, err
	}
	var ent domain.Enterprise
	err := readJSON(filepath.Join(s.enterprisesDir(), id+".json"), &ent)
	if err == nil && (ent.ID != id || domain.ValidateEnterpriseRecord(ent) != nil) {
		return domain.Enterprise{}, domain.ErrIntegrity
	}
	return ent, err
}

func (s *FSStore) UpdateEnterprise(_ context.Context, ent domain.Enterprise) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	var current domain.Enterprise
	if err := readJSON(filepath.Join(s.enterprisesDir(), ent.ID+".json"), &current); err != nil {
		return err
	}
	// Identity and namespace ownership are immutable through this operation.
	// Namespace renames require alias-aware migration and are deliberately a
	// separate use case.
	if current.NamespaceID != ent.NamespaceID || current.Slug != ent.Slug || current.CreatedBy != ent.CreatedBy || !current.CreatedAt.Equal(ent.CreatedAt) {
		return domain.ErrConflict
	}
	data, _ := json.Marshal(ent)
	return writeAtomic(filepath.Join(s.enterprisesDir(), ent.ID+".json"), data)
}

func (s *FSStore) UpdateEnterpriseWithAudit(ctx context.Context, ent domain.Enterprise, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, ent.ID, "enterprise.profile.updated", "enterprise", ent.ID); err != nil {
		return err
	}
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetEnterprise(ctx, ent.ID)
	if err != nil {
		return err
	}
	if err := s.UpdateEnterprise(ctx, ent); err != nil {
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		if rollbackErr := s.UpdateEnterprise(ctx, previous); rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetEnterpriseBySlug(ctx context.Context, slug string) (domain.Enterprise, error) {
	ns, err := s.GetNamespaceBySlug(ctx, slug)
	if err != nil || ns.Kind != domain.NamespaceEnterprise {
		if err == nil {
			err = domain.ErrNotFound
		}
		return domain.Enterprise{}, err
	}
	return s.GetEnterprise(ctx, ns.EnterpriseID)
}

func (s *FSStore) ListEnterprisesForUser(ctx context.Context, userID string) ([]domain.Enterprise, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.enterpriseMembersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Enterprise{}, nil
		}
		return nil, err
	}
	var out []domain.Enterprise
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := s.enterpriseMembership(entry.Name(), userID); err != nil {
			continue
		}
		if ent, err := s.GetEnterprise(ctx, entry.Name()); err == nil {
			out = append(out, ent)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *FSStore) AddEnterpriseMember(ctx context.Context, m domain.EnterpriseMembership) error {
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	return s.addEnterpriseMemberUnlocked(ctx, m)
}

func (s *FSStore) addEnterpriseMemberUnlocked(_ context.Context, m domain.EnterpriseMembership) error {
	if err := domain.ValidateEnterpriseMembershipRecord(m); err != nil {
		return err
	}
	previous, err := s.enterpriseMembership(m.EnterpriseID, m.UserID)
	if err == nil {
		if previous.Role == domain.EnterpriseOwner && m.Role != domain.EnterpriseOwner {
			if err := s.requireAnotherEnterpriseOwner(m.EnterpriseID, m.UserID); err != nil {
				return err
			}
		}
		m.CreatedAt = previous.CreatedAt
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	m.User = nil
	data, _ := json.Marshal(m)
	return writeAtomic(filepath.Join(s.enterpriseMembersDir(), m.EnterpriseID, opaqueName(m.UserID)+".json"), data)
}

func (s *FSStore) AddEnterpriseMemberWithAudit(ctx context.Context, member domain.EnterpriseMembership, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, member.EnterpriseID, "enterprise.member.updated", "user", member.UserID); err != nil {
		return err
	}
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, previousErr := s.GetEnterpriseMembership(ctx, member.EnterpriseID, member.UserID)
	if previousErr != nil && !errors.Is(previousErr, domain.ErrNotFound) {
		return previousErr
	}
	if err := s.addEnterpriseMemberUnlocked(ctx, member); err != nil {
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		var rollbackErr error
		if previousErr == nil {
			rollbackErr = s.addEnterpriseMemberUnlocked(ctx, previous)
		} else {
			rollbackErr = s.removeEnterpriseMemberUnlocked(ctx, member.EnterpriseID, member.UserID)
		}
		if rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) enterpriseMembership(enterpriseID, userID string) (domain.EnterpriseMembership, error) {
	var m domain.EnterpriseMembership
	err := readJSON(filepath.Join(s.enterpriseMembersDir(), enterpriseID, opaqueName(userID)+".json"), &m)
	if err == nil && domain.ValidateEnterpriseMembershipRecord(m) != nil {
		return m, domain.ErrIntegrity
	}
	return m, err
}

func (s *FSStore) requireAnotherEnterpriseOwner(enterpriseID, excludingUserID string) error {
	entries, err := os.ReadDir(filepath.Join(s.enterpriseMembersDir(), enterpriseID))
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
		var member domain.EnterpriseMembership
		if err := readJSON(filepath.Join(s.enterpriseMembersDir(), enterpriseID, entry.Name()), &member); err != nil {
			return err
		}
		if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil || member.EnterpriseID != enterpriseID {
			return domain.ErrIntegrity
		}
		if member.UserID != excludingUserID && member.Role == domain.EnterpriseOwner {
			return nil
		}
	}
	return domain.ErrConflict
}

func (s *FSStore) GetEnterpriseMembership(_ context.Context, enterpriseID, userID string) (domain.EnterpriseMembership, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.EnterpriseMembership{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.EnterpriseMembership{}, err
	}
	return s.enterpriseMembership(enterpriseID, userID)
}

func (s *FSStore) RemoveEnterpriseMember(ctx context.Context, enterpriseID, userID string) error {
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	return s.removeEnterpriseMemberUnlocked(ctx, enterpriseID, userID)
}

func (s *FSStore) removeEnterpriseMemberUnlocked(_ context.Context, enterpriseID, userID string) error {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	previous, previousErr := s.enterpriseMembership(enterpriseID, userID)
	if previousErr != nil && !errors.Is(previousErr, domain.ErrNotFound) {
		return previousErr
	}
	if previousErr == nil && previous.Role == domain.EnterpriseOwner {
		if err := s.requireAnotherEnterpriseOwner(enterpriseID, userID); err != nil {
			return err
		}
	}
	err := os.Remove(filepath.Join(s.enterpriseMembersDir(), enterpriseID, opaqueName(userID)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *FSStore) RemoveEnterpriseMemberWithAudit(ctx context.Context, enterpriseID, userID string, event domain.EnterpriseAuditEvent) error {
	if err := validateEnterpriseMutationAudit(event, enterpriseID, "enterprise.member.removed", "user", userID); err != nil {
		return err
	}
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetEnterpriseMembership(ctx, enterpriseID, userID)
	if err != nil {
		return err
	}
	if err := s.removeEnterpriseMemberUnlocked(ctx, enterpriseID, userID); err != nil {
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		if rollbackErr := s.addEnterpriseMemberUnlocked(ctx, previous); rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) ListEnterpriseMembers(ctx context.Context, enterpriseID string) ([]domain.EnterpriseMembership, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.enterpriseMembersDir(), enterpriseID))
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.EnterpriseMembership{}, nil
		}
		return nil, err
	}
	var out []domain.EnterpriseMembership
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var m domain.EnterpriseMembership
		if readJSON(filepath.Join(s.enterpriseMembersDir(), enterpriseID, entry.Name()), &m) != nil {
			continue
		}
		if domain.ValidateEnterpriseMembershipRecord(m) != nil {
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

func (s *FSStore) PutEnterprisePolicy(_ context.Context, p domain.EnterprisePolicy) error {
	if err := domain.ValidateEnterprisePolicy(p); err != nil {
		return err
	}
	data, _ := json.Marshal(p)
	return writeAtomic(filepath.Join(s.enterprisePoliciesDir(), p.EnterpriseID+".json"), data)
}

func (s *FSStore) PutEnterprisePolicyWithAudit(ctx context.Context, policy domain.EnterprisePolicy, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterprisePolicy(policy); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, policy.EnterpriseID, "enterprise.policy.updated", "enterprise", policy.EnterpriseID); err != nil {
		return err
	}
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	previous, err := s.GetEnterprisePolicy(ctx, policy.EnterpriseID)
	if err != nil {
		return err
	}
	if err := s.PutEnterprisePolicy(ctx, policy); err != nil {
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		if rollbackErr := s.PutEnterprisePolicy(ctx, previous); rollbackErr != nil {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetEnterprisePolicy(_ context.Context, enterpriseID string) (domain.EnterprisePolicy, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.EnterprisePolicy{}, err
	}
	var p domain.EnterprisePolicy
	err := readJSON(filepath.Join(s.enterprisePoliciesDir(), enterpriseID+".json"), &p)
	if err == nil && domain.ValidateEnterprisePolicy(p) != nil {
		return p, domain.ErrIntegrity
	}
	return p, err
}

func (s *FSStore) ListWorkspacesForNamespace(_ context.Context, namespaceID string) ([]domain.Workspace, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.workspacesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Workspace{}, nil
		}
		return nil, err
	}
	var out []domain.Workspace
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var ws domain.Workspace
		if readJSON(filepath.Join(s.workspacesDir(), entry.Name()), &ws) == nil && ws.OwnerNamespaceID == namespaceID {
			if domain.ValidateWorkspaceRecord(ws) != nil {
				return nil, domain.ErrIntegrity
			}
			out = append(out, ws)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *FSStore) CreateEnterpriseWorkspaceWithAudit(ctx context.Context, workspace domain.Workspace, owner domain.Membership, event domain.EnterpriseAuditEvent) error {
	if err := validateEnterpriseWorkspaceMutation(workspace, owner, event); err != nil {
		return err
	}
	namespace, err := s.GetNamespace(ctx, workspace.OwnerNamespaceID)
	if err != nil || namespace.Kind != domain.NamespaceEnterprise || namespace.EnterpriseID != event.EnterpriseID {
		return domain.ErrValidation
	}
	lock := s.enterpriseMutationLock()
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.GetWorkspace(ctx, workspace.ID); !errors.Is(err, domain.ErrNotFound) {
		if err == nil {
			return domain.ErrConflict
		}
		return err
	}
	if err := s.CreateWorkspace(ctx, workspace); err != nil {
		return err
	}
	if err := s.AddMember(ctx, owner); err != nil {
		if rollbackErr := os.Remove(filepath.Join(s.workspacesDir(), workspace.ID+".json")); rollbackErr != nil && !errors.Is(rollbackErr, os.ErrNotExist) {
			return domain.ErrIntegrity
		}
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		memberErr := s.RemoveMember(ctx, owner.WorkspaceID, owner.UserID)
		workspaceErr := os.Remove(filepath.Join(s.workspacesDir(), workspace.ID+".json"))
		if memberErr != nil || workspaceErr != nil && !errors.Is(workspaceErr, os.ErrNotExist) {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) AppendEnterpriseAudit(_ context.Context, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil {
		return err
	}
	data, _ := json.Marshal(event)
	name := event.CreatedAt.UTC().Format("20060102T150405.000000000Z") + "-" + event.ID + ".json"
	return writeAtomic(filepath.Join(s.enterpriseAuditDir(), event.EnterpriseID, name), data)
}

func (s *FSStore) ListEnterpriseAudit(_ context.Context, enterpriseID string, limit int) ([]domain.EnterpriseAuditEvent, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, err := os.ReadDir(filepath.Join(s.enterpriseAuditDir(), enterpriseID))
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.EnterpriseAuditEvent{}, nil
		}
		return nil, err
	}
	var out []domain.EnterpriseAuditEvent
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		entry := entries[i]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var event domain.EnterpriseAuditEvent
		if readJSON(filepath.Join(s.enterpriseAuditDir(), enterpriseID, entry.Name()), &event) == nil {
			out = append(out, event)
		}
	}
	return out, nil
}

func (s *FSStore) CreateBreakGlassGrant(_ context.Context, grant domain.BreakGlassGrant) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	data, _ := json.Marshal(grant)
	return writeAtomic(filepath.Join(s.breakGlassDir(), grant.EnterpriseID, grant.WorkspaceID, opaqueName(grant.UserID), grant.ID+".json"), data)
}

func (s *FSStore) CreateBreakGlassGrantWithAudit(ctx context.Context, grant domain.BreakGlassGrant, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil || event.EnterpriseID != grant.EnterpriseID || event.ActorID != grant.UserID || event.Action != "enterprise.break_glass.created" {
		return domain.ErrValidation
	}
	if err := s.CreateBreakGlassGrant(ctx, grant); err != nil {
		return err
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		grantPath := filepath.Join(s.breakGlassDir(), grant.EnterpriseID, grant.WorkspaceID, opaqueName(grant.UserID), grant.ID+".json")
		if rollbackErr := os.Remove(grantPath); rollbackErr != nil && !os.IsNotExist(rollbackErr) {
			return domain.ErrIntegrity
		}
		return err
	}
	return nil
}

func (s *FSStore) GetActiveBreakGlassGrant(_ context.Context, enterpriseID, workspaceID, userID string, now time.Time) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	entries, err := os.ReadDir(filepath.Join(s.breakGlassDir(), enterpriseID, workspaceID, opaqueName(userID)))
	if err != nil {
		return domain.BreakGlassGrant{}, domain.ErrNotFound
	}
	var latest domain.BreakGlassGrant
	for _, entry := range entries {
		var grant domain.BreakGlassGrant
		if readJSON(filepath.Join(s.breakGlassDir(), enterpriseID, workspaceID, opaqueName(userID), entry.Name()), &grant) != nil {
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

func (s *FSStore) UseActiveBreakGlassGrant(ctx context.Context, enterpriseID, workspaceID, userID string, now time.Time, event domain.EnterpriseAuditEvent) (domain.BreakGlassGrant, error) {
	grant, err := s.GetActiveBreakGlassGrant(ctx, enterpriseID, workspaceID, userID, now)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	event.Reason = grant.Reason
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil || event.EnterpriseID != enterpriseID || event.ActorID != userID || event.Action != "enterprise.break_glass.used" || event.TargetID != workspaceID {
		return domain.BreakGlassGrant{}, domain.ErrValidation
	}
	if err := s.AppendEnterpriseAudit(ctx, event); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}
