package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func hasPRSourcePublication(j domain.PRPromotionJob, events []domain.HistoryEvent) bool {
	for _, e := range events {
		if e.RepoID == string(j.RepoID) && e.GitAfter == j.PR.HeadSHA && e.Target != "" &&
			domain.MatchesPRSourcePublication(e, j.PR.HeadBranch, events) {
			return true
		}
	}
	return false
}
