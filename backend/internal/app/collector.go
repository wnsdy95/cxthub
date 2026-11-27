package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) collectorStore() (outbound.CollectorStore, error) {
	store, ok := s.meta.(outbound.CollectorStore)
	if !ok {
		return nil, fmt.Errorf("%w: collector storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// ReportCollector takes what the collector on one of the caller's machines
// says about itself: which pushes its clone made, and what went wrong in its
// run. It moves the repository revision, because the Sessions tab shows what
// follows from it.
func (s *Service) ReportCollector(ctx context.Context, repo domain.ContentHash, report domain.CollectorReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	ctx = auditOperation(ctx, "collector.reported")
	_, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (struct{}, error) {
		store, err := s.collectorStore()
		if err != nil {
			return struct{}{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return struct{}{}, err
		}
		collector, claims := report.Heard(actor, time.Now())
		return struct{}{}, store.PutCollectorReport(bound, repo, collector, claims)
	})
	return err
}

// PushOrigins says where the newest pushes of the repository came from, to
// whoever may view it: the pushes collectors claimed and the pushes the
// server saw on the Git host.
func (s *Service) PushOrigins(ctx context.Context, repo domain.ContentHash) (domain.PushOriginView, error) {
	type read struct {
		view       domain.PushOriginView
		collectors []domain.ReportedCollector
		reader     string
	}
	got, err := repositoryRead(ctx, s, func(bound context.Context) (read, error) {
		store, err := s.collectorStore()
		if err != nil {
			return read{}, err
		}
		reader, err := anchoredReadActor(bound)
		if err != nil {
			return read{}, err
		}
		collectors, err := store.ListReportedCollectors(bound, repo)
		if err != nil {
			return read{}, err
		}
		claims, err := store.ListClaimedPushes(bound, repo, domain.PushOriginLimit)
		if err != nil {
			return read{}, err
		}
		seen := []domain.RemoteBranchPush{}
		if log, ok := s.meta.(outbound.RemotePushStore); ok {
			if seen, err = log.ListRecentRemotePushes(bound, repo, domain.PushOriginLimit); err != nil {
				return read{}, err
			}
		}
		return read{domain.PushOriginView{Pushes: domain.PushOrigins(seen, claims, collectors, time.Now())}, collectors, reader}, nil
	})
	if err != nil {
		return domain.PushOriginView{}, err
	}
	accounts := map[string]struct{}{}
	for _, collector := range got.collectors {
		accounts[collector.Account] = struct{}{}
	}
	got.view.Collectors = domain.PushCollectors(got.collectors, got.reader, s.anchoredAccountNames(ctx, accounts))
	return got.view, nil
}

var _ inbound.CollectorReports = (*Service)(nil)
