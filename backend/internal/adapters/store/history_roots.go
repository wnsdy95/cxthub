package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func historyRoots(e domain.HistoryEvent) []domain.Ref {
	out := []domain.Ref{}
	for _, item := range []struct {
		name string
		id   domain.ContentHash
	}{{"shared-target", e.SharedTarget}, {"source", e.Source}, {"target", e.Target}, {"memory-source", e.MemorySource}} {
		if item.id != "" {
			out = append(out, domain.Ref{RepoID: domain.ContentHash(e.RepoID), Kind: domain.RefTag, Name: "cxt/history/v1/" + e.ID + "/" + item.name, Target: item.id})
		}
	}
	return out
}
