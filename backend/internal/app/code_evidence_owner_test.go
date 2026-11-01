package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestCodeEvidenceUsesOneBoundOwnerAcrossPolicyAndMemory(test *testing.T) {
	fixture := newEffectiveFixture(test)
	ctx := systemTestContext()
	evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	if evidence.git != nil || evidence.ReadCount() != 0 || evidence.Limited() || evidence.TraversalLimited() {
		test.Fatal("Git evidence was read or allocated before demand")
	}
	policy := branchPolicyEvidence(evidence)
	if relation, err := policy.Relation(ctx, effectiveOID(2), effectiveOID(3)); err != nil || relation != "ancestor" {
		test.Fatal("policy could not read bound evidence", relation, err)
	}
	owner := evidence.git
	if owner == nil || evidence.ReadCount() != 1 {
		test.Fatal("policy did not use the request owner", evidence.ReadCount())
	}
	fixture.svc.meta = &poisonedGraphMetadata{}
	selection := domain.CodeSelection{CodeCommit: effectiveOID(3), SourceCommit: effectiveOID(2), Paths: []string{"a", "b"}}
	first, err := evidence.query(ctx, selection, domain.RepositoryRevision{})
	if err != nil || first.Relation != "ancestor" || len(first.Paths) != 2 {
		test.Fatal("memory path rediscovered the parent or a replacement reader", first, err)
	}
	reads := evidence.ReadCount()
	if reads <= 1 || owner.ReadCount() != reads || owner != evidence.git {
		test.Fatal("memory path copied the Git cache or read budget", reads, owner.ReadCount())
	}
	if _, err := owner.GetGitCommitTree(ctx, evidence.repo, evidence.origin, effectiveOID(3)); err != nil || evidence.ReadCount() != reads {
		test.Fatal("policy and memory did not share cached commits", err, evidence.ReadCount())
	}
	if err := evidence.bindContributionHistory(evidence.repo, []domain.HistoryEvent{{ID: "new input"}}); err != nil {
		test.Fatal(err)
	}
	if _, err := evidence.query(ctx, selection, domain.RepositoryRevision{}); err != nil || evidence.git != owner || evidence.ReadCount() != reads {
		test.Fatal("ledger binding reset the independent Git cache", err, evidence.ReadCount())
	}
}

func TestCodeEvidenceRejectsMismatchedRepositoryMetadata(test *testing.T) {
	repo := hh("Git evidence metadata scope")
	service := &Service{meta: &capturedGraphRepositoryOnly{metadata: domain.Repo{ID: hh("another repository")}}}
	evidence, err := service.newCodeEvidence(systemTestContext(), repo)
	if evidence != nil || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("mismatched repository created a scoped evidence reader", evidence, err)
	}
}

func TestCodeEvidenceCancellationPrecedesLazyInitialization(test *testing.T) {
	evidence := &codeEvidence{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := evidence.GetGitCommitTree(ctx, "", "", ""); !errors.Is(err, context.Canceled) || evidence.git != nil {
		test.Fatal("canceled lookup initialized or masked the reader", err)
	}
	if _, err := evidence.relation(ctx, "", ""); !errors.Is(err, context.Canceled) || evidence.git != nil {
		test.Fatal("canceled policy initialized or masked the reader", err)
	}
}
