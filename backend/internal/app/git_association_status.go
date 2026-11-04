package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.GitAssociationStatusQuery = (*Service)(nil)
var _ inbound.GitAssociationStatusQuery = (*GitAssociationStatusQueries)(nil)

type gitAssociationDiscoveryReader interface {
	GetGitAssociationDiscovery(context.Context, domain.ContentHash, string) (domain.GitAssociationDiscovery, error)
}

type GitAssociationStatusQueries struct {
	scope      evidencequery.Scope
	metadata   evidencequery.MetadataReader
	statuses   outbound.GitAssociationStatusStore
	discovery  gitAssociationDiscoveryReader
	heads      evidencequery.HeadScanReader
	atomicRead bool
}

type gitAssociationStatusLegacyScope struct {
	work *repositoryWork
}

func (scope *gitAssociationStatusLegacyScope) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	_, err := repositoryReadForRepoWithWork(ctx, scope.work, repo, func(bound context.Context) (struct{}, error) {
		return struct{}{}, read(bound)
	})
	return err
}

func newGitAssociationStatusQueries(metadata evidencequery.MetadataReader, scope evidencequery.Scope, atomicRead bool) (*GitAssociationStatusQueries, error) {
	if !rootAdapterPresent(metadata) || !rootAdapterPresent(scope) {
		return nil, domain.ErrValidation
	}
	statuses, _ := metadata.(outbound.GitAssociationStatusStore)
	discovery, _ := metadata.(gitAssociationDiscoveryReader)
	heads, _ := metadata.(evidencequery.HeadScanReader)
	for _, reader := range []any{statuses, discovery, heads} {
		if reader != nil && !rootAdapterPresent(reader) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	return &GitAssociationStatusQueries{scope: scope, metadata: metadata, statuses: statuses, discovery: discovery, heads: heads, atomicRead: atomicRead}, nil
}

func (service *Service) GetGitAssociationStatus(ctx context.Context, repo domain.ContentHash, request domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error) {
	work := service.repositoryWork()
	queries, err := newGitAssociationStatusQueries(service.meta, &gitAssociationStatusLegacyScope{work: work}, work.transactions != nil)
	if err != nil {
		return domain.GitAssociationStatusPage{}, err
	}
	return queries.GetGitAssociationStatus(ctx, repo, request)
}

func (queries *GitAssociationStatusQueries) GetGitAssociationStatus(ctx context.Context, repo domain.ContentHash, request domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error) {
	if domain.ValidateContentHash(repo) != nil || request.Validate() != nil {
		return domain.GitAssociationStatusPage{}, domain.ErrValidation
	}
	var page domain.GitAssociationStatusPage
	err := queries.scope.ReadRepository(ctx, repo, func(read context.Context) error {
		var err error
		page, err = queries.readStatus(read, repo, request)
		return err
	})
	if err != nil && queries.atomicRead {
		return domain.GitAssociationStatusPage{}, err
	}
	return page, err
}

func (queries *GitAssociationStatusQueries) readStatus(read context.Context, repo domain.ContentHash, request domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error) {
	page := domain.GitAssociationStatusPage{RepoID: repo, Kind: request.Kind, Filter: request.State, Items: []domain.GitAssociationStatus{},
		Discovery: domain.GitAssociationDiscoveryStatus{State: "unavailable"}, Reconciliation: domain.GitAssociationHeadStatus{State: "unavailable"}, CurrentApplicability: "not_evaluated"}
	if queries.statuses == nil {
		return page, domain.ErrGitAssociationStatusUnavailable
	}
	metadata, err := queries.metadata.GetRepo(read, repo)
	if err != nil {
		return page, err
	}
	if metadata.GitRemoteURL == "" {
		page.Discovery.State = "no_origin"
		return page, nil
	}
	if err := queries.gitAssociationDiscoveryStatus(read, metadata, &page); err != nil {
		return page, err
	}
	evidence, err := queries.statuses.ReadGitAssociationStatus(read, repo, metadata.GitRemoteURL, request)
	if err != nil {
		return page, err
	}
	if len(evidence) > request.Limit+1 {
		return page, domain.ErrIntegrity
	}
	now, previous := time.Now().UTC(), request.Cursor
	for _, row := range evidence {
		if err := read.Err(); err != nil {
			return page, err
		}
		status, err := projectGitAssociationStatus(repo, metadata.GitRemoteURL, request.Kind, row, now)
		if err != nil {
			return page, err
		}
		if status.ID <= previous || (request.State == "pending" && (status.State == "completed" || status.State == "admitted")) {
			return page, domain.ErrIntegrity
		}
		previous = status.ID
		page.Items = append(page.Items, status)
	}
	if len(page.Items) > request.Limit {
		page.NextCursor = page.Items[request.Limit-1].ID
		page.Items = page.Items[:request.Limit]
	}
	return page, nil
}

func (queries *GitAssociationStatusQueries) gitAssociationDiscoveryStatus(ctx context.Context, repo domain.Repo, page *domain.GitAssociationStatusPage) error {
	if queries.discovery != nil {
		checkpoint, err := queries.discovery.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			page.Discovery.State = "not_started"
		case err != nil:
			return err
		default:
			if checkpoint.Validate() != nil || checkpoint.RepoID != repo.ID || checkpoint.GitOrigin != repo.GitRemoteURL {
				return domain.ErrIntegrity
			}
			page.Discovery.State, page.Discovery.Version = "scanning", checkpoint.Version
			if checkpoint.Cursor == "" && checkpoint.Checkpoint != nil {
				page.Discovery.State = "checkpoint_recorded"
			}
		}
	}
	if queries.heads != nil {
		job, err := queries.heads.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			page.Reconciliation.State = "not_started"
		case err != nil:
			return err
		default:
			if job.Validate() != nil || job.RepoID != repo.ID || job.GitOrigin != repo.GitRemoteURL {
				return domain.ErrIntegrity
			}
			page.Reconciliation = domain.GitAssociationHeadStatus{State: job.State, Page: job.Page, Reason: job.Reason}
			if job.State == "running" && !job.LeaseUntil.After(time.Now()) {
				page.Reconciliation.State, page.Reconciliation.Reason = "retrying", "lease_expired"
			}
		}
	}
	return ctx.Err()
}

func projectGitAssociationStatus(repo domain.ContentHash, origin, kind string, row outbound.GitAssociationStatusEvidence, now time.Time) (domain.GitAssociationStatus, error) {
	status := domain.GitAssociationStatus{Kind: kind}
	if kind == "publication" {
		publication := row.Publication
		if publication == nil || domain.ValidateHistoryEvent(*publication) != nil || publication.Kind != "publish" || publication.RepoID != string(repo) {
			return status, domain.ErrIntegrity
		}
		status.ID, status.PublicationID, status.Capture, status.OriginalHead = publication.ID, publication.ID, publication.Target, publication.GitAfter
		status.SourceBranch, status.SourceBranchID = publication.Branch, publication.BranchID
		if publication.LocalBranch != "" {
			status.SourceBranch = publication.LocalBranch
		}
		status.State, status.Reason = "discovered", "waiting_admission"
		if row.Admission != nil {
			admission := *row.Admission
			if admission.Validate() != nil || admission.Destination.RepoID != repo || admission.Destination.GitOrigin != origin || admission.PublicationID != publication.ID || admission.SourceBranchID != publication.BranchID || admission.Capture != publication.Target || admission.OriginalHead != publication.GitAfter {
				return status, domain.ErrIntegrity
			}
			status.State, status.Reason, status.AdmissionID = "admitted", "", admission.ID
		}
		return status, nil
	}
	if row.Admission == nil || row.Admission.Validate() != nil || row.Admission.Destination.RepoID != repo || row.Admission.Destination.GitOrigin != origin {
		return status, domain.ErrIntegrity
	}
	admission := *row.Admission
	status.PublicationID, status.AdmissionID = admission.PublicationID, admission.ID
	status.Capture, status.OriginalHead = admission.Capture, admission.OriginalHead
	status.SourceBranch, status.SourceBranchID = admission.SourceBranch, admission.SourceBranchID
	status.DestinationBranch, status.DestinationBranchID = admission.Destination.DestinationBranch, admission.Destination.DestinationBranchID
	if row.Proof != nil {
		proof := *row.Proof
		record := proof.Contribution
		if proof.Validate() != nil || proof.AdmissionID != admission.ID || record.RepoID != repo || record.GitOrigin != origin || record.ProviderRepositoryID != admission.Destination.ProviderRepositoryID || record.SourceBranchID != admission.SourceBranchID || record.DestinationBranchID != admission.Destination.DestinationBranchID || record.Capture != admission.Capture || record.OriginalHead != admission.OriginalHead {
			return status, domain.ErrIntegrity
		}
		status.ContributionID = record.ID
	}
	var next, lease time.Time
	completed := false
	switch kind {
	case "integration":
		if row.Work == nil || row.Work.ValidateRecorded() != nil || row.Work.RepoID != repo || row.Work.AdmissionID != admission.ID || row.Work.AfterReadVersion != admission.AfterReadVersion {
			return status, domain.ErrIntegrity
		}
		work := *row.Work
		status.ID, status.State, status.Reason, status.Attempts = work.AdmissionID, work.State, work.Reason, work.Attempts
		next, lease, completed = work.NextAttempt, work.LeaseUntil, row.Proof != nil
	case "memory":
		if row.Supplement == nil || row.Supplement.Validate() != nil || row.Proof == nil || row.Publication == nil {
			return status, domain.ErrIntegrity
		}
		work, publication := *row.Supplement, *row.Publication
		if work.RepoID != repo || work.GitOrigin != origin || work.ProofID != row.Proof.ID || work.ContributionID != status.ContributionID || work.PublicationID != publication.ID || domain.ValidateHistoryEvent(publication) != nil || publication.Kind != "publish" || publication.RepoID != string(repo) || !publication.MemoryPinned || publication.BranchID != admission.SourceBranchID || publication.Target != admission.Capture || publication.Source != admission.Capture || publication.GitAfter != admission.OriginalHead {
			return status, domain.ErrIntegrity
		}
		status.ID, status.PublicationID, status.State, status.Reason, status.Attempts = work.ID, publication.ID, work.State, work.Reason, work.Attempts
		next, lease = work.NextAttempt, work.LeaseUntil
		if row.Memory != nil {
			source := publication.MemorySource
			if source == "" && publication.MemoryHash != "" {
				source = admission.Capture
			}
			want := (domain.GitContributionMemory{RepoID: repo, ContributionID: work.ContributionID, PublicationID: work.PublicationID, MemoryHash: publication.MemoryHash, MemorySource: source, MemoryPinned: true}).WithID()
			if row.Memory.Validate() != nil || *row.Memory != want {
				return status, domain.ErrIntegrity
			}
			completed = true
		}
	default:
		return status, domain.ErrValidation
	}
	if completed {
		status.State, status.Reason = "completed", ""
	} else if status.State == "completed" {
		return status, domain.ErrIntegrity
	} else if status.State == "processing" && !lease.After(now) {
		status.State, status.Reason = "retrying", "lease_expired"
		next = now
	}
	if status.State == "waiting" || status.State == "retrying" {
		status.NextAttempt = &next
	}
	return status, nil
}
