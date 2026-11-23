package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type auditReader struct {
	err    error
	calls  int
	during func()
	prs    []domain.PullRequestMerge
}

func (r *auditReader) ReadAuditCommit(_ context.Context, _ string, sha string) (string, error) {
	r.calls++
	if r.during != nil {
		r.during()
		r.during = nil
	}
	return sha, r.err
}
func (r *auditReader) ReadAuditPR(_ context.Context, _ string, _ int) (domain.PullRequestMerge, bool, error) {
	r.calls++
	return domain.PullRequestMerge{}, false, r.err
}
