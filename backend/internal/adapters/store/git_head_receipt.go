package store

import (
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func validateGitHeadReceiptScope(repo domain.ContentHash, origin string, version int64) error {
	if domain.ValidateContentHash(repo) != nil || origin == "" || strings.TrimSpace(origin) != origin || strings.ContainsAny(origin, "\x00\r\n") || version < 0 {
		return domain.ErrValidation
	}
	return nil
}

func validateGitHeadFinish(claim, current domain.GitHeadScan, now time.Time) error {
	if current.Validate() != nil || current.RepoID != claim.RepoID || current.GitOrigin != claim.GitOrigin {
		return domain.ErrIntegrity
	}
	if now.IsZero() || current.State != "running" || claim.State != "running" || current.Version != claim.Version || current.Page != claim.Page || !current.LeaseUntil.Equal(claim.LeaseUntil) || !current.LeaseUntil.After(now) {
		return domain.ErrConflict
	}
	return nil
}

func validateGitHeadReceiptIdentity(receipt domain.GitHeadReceipt, repo domain.ContentHash, origin string, version int64) error {
	if receipt.Validate() != nil || receipt.RepoID != repo || receipt.GitOrigin != origin || receipt.Version != version {
		return domain.ErrIntegrity
	}
	return nil
}
