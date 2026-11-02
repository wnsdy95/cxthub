package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func notifyRefUpdateFromStores(ctx context.Context, metadata outbound.MetadataStore, repositories outbound.RepositoryStore, repoID domain.ContentHash, ref domain.Ref, forced, created bool) error {
	outbox, _ := metadata.(outbound.NotificationStore)
	if outbox != nil && !rootAdapterPresent(outbox) {
		return domain.ErrValidation
	}
	return notifyRefUpdateWithSources(ctx, metadata, repositories, outbox, repoID, ref, forced, created)
}

func notifyRefUpdateWithSources(ctx context.Context, metadata RepositoryAuthorityMetadata, repositories repositoryRefRepositoryReader, outbox RepositoryNotificationQueue, repoID domain.ContentHash, ref domain.Ref, forced, created bool) error {
	if repositories == nil || ref.Kind != domain.RefBranch {
		return nil
	}
	repo, err := metadata.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return nil
	}
	repositoryRecord, err := repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
	}
	name := repo.RemoteURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	verb := "update"
	if created {
		verb = "new branch"
	}
	if forced {
		verb += "(force)"
	}
	return enqueueRepositoryNotificationWithQueue(ctx, outbox, repositoryRecord, "ref_updated", fmt.Sprintf("cxthub: %s — branch %q %s → %s", name, ref.Name, verb, shortHash(ref.Target)))
}

func verifyStoredSnapshotDocument(ctx context.Context, metadata outbound.MetadataStore, blobs outbound.BlobStore, engine outbound.GitEngine, supported []domain.DocumentIdentity, repo domain.ContentHash, snap domain.Snapshot) error {
	return documentPublicationSourcesFromStores(metadata, blobs, engine, supported).VerifySnapshotDocument(ctx, repo, snap)
}

func verifyStoredSnapshotReferenceFromSources(ctx context.Context, blobs outbound.BlobStore, engine outbound.GitEngine, repo domain.ContentHash, snap domain.Snapshot) error {
	return documentPublicationSourcesFromStores(nil, blobs, engine, nil).verifyReference(ctx, repo, snap)
}

func queueHistoryGitScanFromStore(ctx context.Context, metadata outbound.MetadataStore, e domain.HistoryEvent) error {
	st, ok := metadata.(outbound.GitScanStore)
	if !ok {
		return nil
	}
	return queueHistoryGitScanWithSources(ctx, metadata, st, e)
}

func queueHistoryGitScanWithSources(ctx context.Context, metadata RepositoryAuthorityMetadata, scans historyGitScanQueue, e domain.HistoryEvent) error {
	if scans == nil {
		return nil
	}
	repo, err := metadata.GetRepo(ctx, domain.ContentHash(e.RepoID))
	if err != nil {
		return err
	}
	if repo.GitRemoteURL == "" {
		return nil
	}
	// Existing accepted history is also the CLI's durable offline outbox. A
	// duplicate delivery repairs enqueue after a development-FS process crash.
	oids := []string{e.GitBefore, e.GitAfter}
	if e.PR != nil {
		oids = append(oids, e.PR.MergeSHA, e.PR.HeadSHA)
	}
	for _, sha := range oids {
		if sha == "" {
			continue
		}
		if domain.ValidateGitOID(sha) != nil {
			continue
		}
		if err = scans.EnqueueGitScan(ctx, domain.NewGitScan(repo.ID, repo.GitRemoteURL, sha, time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}

func queuePRGitScansFromStore(ctx context.Context, metadata outbound.MetadataStore, repo domain.ContentHash, origin string, pr domain.PullRequestMerge) error {
	st, ok := metadata.(outbound.GitScanStore)
	if !ok {
		return nil
	}
	for _, sha := range []string{pr.HeadSHA, pr.MergeSHA} {
		if domain.ValidateGitOID(sha) != nil {
			continue
		}
		if err := st.EnqueueGitScan(ctx, domain.NewGitScan(repo, origin, sha, time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}
