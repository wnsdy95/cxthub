package app

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestOrganizationAuditPages(t *testing.T) {
	runOrganizationAuditPages(t, store.NewFSStore(t.TempDir()))
}
func runOrganizationAuditPages(t *testing.T, st teamTestStore) {
	ctx := inbound.WithCorrelation(systemTestContext(), "req_test")
	f := makeTeamFixture(t, st)
	now := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	for n := 0; n < 115; n++ {
		if err := st.AppendOrganizationAudit(ctx, organizationAudit(ctx, f.organization.ID, f.owner.ID, "fixture.changed", "repository", f.repository.ID, "", now)); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := st.ListOrganizationAudit(ctx, f.organization.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.OrganizationAuditEvent
	cursor := ""
	for {
		page, err := f.identity.OrganizationAuditPage(ctx, f.owner.ID, f.organization.ID, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Events...)
		if len(got) == 7 { // New records must not shift or duplicate older pages.
			if err = st.AppendOrganizationAudit(ctx, organizationAudit(ctx, f.organization.ID, f.owner.ID, "newer.changed", "repository", f.repository.ID, "", now.Add(time.Minute))); err != nil {
				t.Fatal(err)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(initial) {
		t.Fatalf("pages=%d initial=%d", len(got), len(initial))
	}
	for n := range got {
		if got[n].ID != initial[n].ID {
			t.Fatal("unstable ordering")
		}
		if n < 115 && got[n].CorrelationID != "req_test" {
			t.Fatal("correlation lost")
		}
	}
	if _, err = f.identity.OrganizationAuditPage(ctx, f.member.ID, f.organization.ID, "", 7); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member audit: %v", err)
	}
	if _, err = f.identity.OrganizationAuditPage(ctx, f.owner.ID, f.organization.ID, "malformed", 7); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cursor: %v", err)
	}
	other, err := f.identity.CreateOrganization(ctx, f.owner, "Other audit", "audit-"+domain.NewID("")[:8])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.identity.OrganizationAuditPage(ctx, f.owner.ID, other.ID, domain.EncodeAuditCursor(got[0]), 7); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cross-tenant cursor: %v", err)
	}
}
