//go:build postgres

package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func queryGitStatus(t *testing.T, fixture *gitContributionQueryPGFixture, kind, state, cursor string, limit int) domain.GitAssociationStatusPage {
	t.Helper()
	page, err := fixture.scans.core.GetGitAssociationStatus(systemTestContext(), fixture.basis.repo.ID, domain.GitAssociationStatusRequest{Kind: kind, State: state, Cursor: cursor, Limit: limit})
	if err != nil || page.Items == nil || page.RepoID != fixture.basis.repo.ID || page.Kind != kind || page.Filter != state || page.CurrentApplicability != "not_evaluated" {
		t.Fatalf("status contract: %+v %v", page, err)
	}
	service := fixture.scans.core
	reads, err := NewReadServices(ReadServicesConfig{
		Metadata: service.meta, Blobs: service.blobs, DocumentProfile: inbound.DocumentQueryCompatibility,
		ContextProfile: inbound.ContextQueryPinnedSnapshots, SupportedDocumentIdentities: service.DocumentIdentitiesSupported(),
	})
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := reads.GitAssociationStatus.GetGitAssociationStatus(systemTestContext(), fixture.basis.repo.ID,
		domain.GitAssociationStatusRequest{Kind: kind, State: state, Cursor: cursor, Limit: limit})
	if err != nil || !reflect.DeepEqual(standalone, page) {
		t.Fatal("standalone read composition changed the legacy projection", standalone, page, err)
	}
	return page
}

func TestPGGitAssociationStatusLifecycleWithoutQueryMutations(t *testing.T) {
	fixture, _ := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	checkReadOnly := fixture.assertReadOnly(t)
	pending := queryGitStatus(t, fixture, "integration", "pending", "", 30)
	if len(pending.Items) != 0 {
		t.Fatal("proof-completed work shown as pending", pending.Items)
	}
	completed := queryGitStatus(t, fixture, "integration", "all", "", 30)
	if len(completed.Items) != 1 || completed.Items[0].State != "completed" || completed.Items[0].ContributionID != fixture.proof.Contribution.ID || completed.Items[0].NextAttempt != nil {
		t.Fatal("historical completion hidden by scheduler state", completed.Items)
	}
	memory := queryGitStatus(t, fixture, "memory", "pending", "", 30)
	if len(memory.Items) != 1 || memory.Items[0].PublicationID != publication.ID || memory.Items[0].State != "waiting" || memory.Items[0].NextAttempt == nil {
		t.Fatal("late memory work missing", memory.Items)
	}
	all := queryGitStatus(t, fixture, "publication", "all", "", 100)
	var joined []domain.GitAssociationStatus
	cursor := ""
	for count := 0; count < 20; count++ {
		page := queryGitStatus(t, fixture, "publication", "all", cursor, 1)
		joined = append(joined, page.Items...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor <= cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if len(all.Items) < 2 || !reflect.DeepEqual(joined, all.Items) {
		t.Fatal("pagination dropped or duplicated publications", joined, all.Items)
	}
	checkReadOnly()
	if err := fixture.scans.ProcessContributions(systemTestContext(), 8); err != nil {
		t.Fatal(err)
	}
	defer fixture.assertReadOnly(t)()
	memory = queryGitStatus(t, fixture, "memory", "pending", "", 30)
	if len(memory.Items) != 0 {
		t.Fatal("pinned memory shown as pending", memory.Items)
	}
	memory = queryGitStatus(t, fixture, "memory", "all", "", 30)
	found := false
	for _, item := range memory.Items {
		if item.PublicationID == publication.ID {
			found = item.State == "completed" && item.NextAttempt == nil && item.ContributionID == fixture.proof.Contribution.ID
		}
	}
	if !found {
		t.Fatal("memory completion missing", memory.Items)
	}
}

func TestPGGitAssociationStatusProjectionAuthorityAndCorruption(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	ctx := systemTestContext()
	repo, origin := fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL
	rows, err := storage.ReadGitAssociationStatus(ctx, repo, origin, domain.GitAssociationStatusRequest{Kind: "integration", State: "all", Limit: 30})
	if err != nil || len(rows) != 1 {
		t.Fatalf("integration evidence %+v %v", rows, err)
	}
	row := rows[0]
	now := time.Now().UTC()
	work := *row.Work
	work.Version, work.Attempts, work.State, work.Reason = 1, 1, "processing", "temporary_failure"
	work.LeaseUntil = now.Add(-time.Second)
	row.Work = &work
	status, err := projectGitAssociationStatus(repo, origin, "integration", row, now)
	if err != nil || status.State != "completed" || status.Reason != "" || status.NextAttempt != nil {
		t.Fatal("receipt failed to dominate expired lease", status, err)
	}
	row.Proof = nil
	status, err = projectGitAssociationStatus(repo, origin, "integration", row, now)
	if err != nil || status.State != "retrying" || status.Reason != "lease_expired" || status.NextAttempt == nil || !status.NextAttempt.Equal(now) {
		t.Fatal("expired lease misreported", status, err)
	}
	work.State, work.LeaseUntil = "completed", time.Time{}
	if _, err := projectGitAssociationStatus(repo, origin, "integration", row, now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("scheduler completion invented effect", err)
	}
	row = rows[0]
	proof := *row.Proof
	proof.Contribution.Capture = domain.HashContent([]byte("unrelated capture"))
	proof.Contribution = proof.Contribution.WithID()
	proof = proof.WithID()
	row.Proof = &proof
	if _, err := projectGitAssociationStatus(repo, origin, "integration", row, now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("foreign proof accepted", err)
	}
	if err := fixture.scans.ProcessContributions(ctx, 8); err != nil {
		t.Fatal(err)
	}
	rows, err = storage.ReadGitAssociationStatus(ctx, repo, origin, domain.GitAssociationStatusRequest{Kind: "memory", State: "all", Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	var memoryRow outbound.GitAssociationStatusEvidence
	for _, candidate := range rows {
		if candidate.Publication.ID == publication.ID {
			memoryRow = candidate
		}
	}
	if memoryRow.Memory == nil {
		t.Fatal("missing exact memory effect")
	}
	memory := *memoryRow.Memory
	memory.MemoryHash = domain.ContentHash("sha256:" + strings.Repeat("e", 64))
	memory = memory.WithID()
	memoryRow.Memory = &memory
	if _, err := projectGitAssociationStatus(repo, origin, "memory", memoryRow, now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("different immutable pin accepted", err)
	}
	memoryRow.Memory = nil
	if _, err := projectGitAssociationStatus(repo, origin, "memory", memoryRow, now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("completed supplement without pin accepted", err)
	}
}
