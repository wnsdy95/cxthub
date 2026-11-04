package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func validateGitContributionAdmissionIdentity(admission domain.GitContributionAdmission, repo domain.ContentHash, id, key, origin, receiptID string) error {
	if admission.ID != id || admission.Key() != key || admission.Destination.RepoID != repo || admission.Destination.GitOrigin != origin || admission.Destination.ReadReceiptID != receiptID {
		return domain.ErrIntegrity
	}
	return nil
}

func validateGitContextHeadIdentity(head domain.GitContextHead, repo domain.ContentHash, id, origin, branchID, branch string, version int64, receiptID string) error {
	destination := head.Destination
	if head.ID != id || destination.RepoID != repo || destination.GitOrigin != origin || destination.DestinationBranchID != branchID || destination.DestinationBranch != branch || destination.ReadVersion != version || destination.ReadReceiptID != receiptID {
		return domain.ErrIntegrity
	}
	return nil
}

func validateGitContextHeadReceipt(head domain.GitContextHead, receipt domain.GitHeadReceipt, repo domain.ContentHash, origin string, version int64, receiptID string) error {
	if validateGitHeadReceiptIdentity(receipt, repo, origin, version) != nil || receipt.ID != receiptID || !head.Destination.MatchesReceipt(receipt) {
		return domain.ErrIntegrity
	}
	return nil
}

func validateGitContributionProofIdentity(proof domain.GitContributionProof, repo domain.ContentHash, id, admissionID, headID, contributionID string) error {
	if proof.ID != id || proof.AdmissionID != admissionID || proof.ContextHeadID != headID || proof.Contribution.RepoID != repo || proof.Contribution.ID != contributionID {
		return domain.ErrIntegrity
	}
	return nil
}

func validateGitContributionProofBaseline(admission domain.GitContributionAdmission, baseline domain.GitHeadReceipt, repo domain.ContentHash, origin string, version int64, receiptID string) error {
	if validateGitHeadReceiptIdentity(baseline, repo, origin, version) != nil || baseline.ID != receiptID || !admission.MatchesBaseline(baseline) {
		return domain.ErrIntegrity
	}
	return nil
}

func validateGitContributionProofInputs(proof domain.GitContributionProof, admission domain.GitContributionAdmission, head domain.GitContextHead, receipt domain.GitHeadReceipt, contribution domain.GitContribution) error {
	if contribution != proof.Contribution || !proof.MatchesInputs(admission, head, receipt) {
		return domain.ErrIntegrity
	}
	return nil
}
