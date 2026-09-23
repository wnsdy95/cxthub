package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
	"time"
)

type usageStore struct {
	*store.FSStore
	reads, reconciles int
}

func (s *usageStore) ReadStorageUsage(_ context.Context, ns string, _, _ time.Time) (domain.StorageUsage, error) {
	s.reads++
	return domain.StorageUsage{NamespaceID: ns}, nil
}
func (s *usageStore) ReconcileStorageUsage(context.Context, string) error { s.reconciles++; return nil }
func (s *usageStore) ConfigureStoragePolicy(context.Context, string, string, int64, domain.StoragePolicy, string, string) error {
	return errors.New("not exposed to customers")
}
func TestStorageAuthorityDoesNotGrantContextOrPlanAdministration(t *testing.T) {
	ctx := systemTestContext()
	st := &usageStore{FSStore: store.NewFSStore(t.TempDir())}
	svc := NewIdentityService(nil, st)
	owner := domain.User{ID: "owner", Username: "owner", Name: "Owner", Email: "owner@test.example"}
	for _, u := range []domain.User{owner, {ID: "admin", Username: "admin-user", Name: "Admin", Email: "admin@test.example"}, {ID: "member", Username: "member-user", Name: "Member", Email: "member@test.example"}} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	ent, err := svc.CreateOrganization(ctx, owner, "Metering", "metering")
	if err != nil {
		t.Fatal(err)
	}
	for u, role := range map[string]domain.OrganizationRole{"admin": domain.OrganizationAdmin, "member": domain.OrganizationMember} {
		if err := svc.UpdateOrganizationMember(ctx, owner.ID, ent.ID, u, role); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, u := range []string{"member", "outsider"} {
		if _, err := svc.StorageUsage(ctx, u, ent.NamespaceID, now.Add(-time.Hour), now); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s read=%v", u, err)
		}
	}
	if _, err := svc.StorageUsage(ctx, "admin", ent.NamespaceID, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcileStorageUsage(ctx, "admin", ent.NamespaceID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("admin recounted", err)
	}
	if err := svc.ReconcileStorageUsage(ctx, "owner", ent.NamespaceID); err != nil {
		t.Fatal(err)
	}
	ns, err := svc.ensurePersonalNamespace(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StorageUsage(ctx, "admin", ns.ID, now.Add(-time.Hour), now); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("personal usage exposed", err)
	}
	if _, err := svc.StorageUsage(ctx, "owner", "self", now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if st.reads != 2 || st.reconciles != 1 {
		t.Fatal("unauthorized storage work", st.reads, st.reconciles)
	}
}
