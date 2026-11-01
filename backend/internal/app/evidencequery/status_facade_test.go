package evidencequery_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type statusFacadeSnapshotKey struct{}

type statusFacadeReader struct {
	metadata        domain.Repo
	job             domain.GitChangeJob
	summaries       []domain.GitChangeSummary
	repoError       error
	metadataReads   int
	changeReads     int
	requireSnapshot bool
}

func (reader *statusFacadeReader) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	reader.metadataReads++
	if reader.requireSnapshot && ctx.Value(statusFacadeSnapshotKey{}) != reader {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return reader.metadata, reader.repoError
}

func (reader *statusFacadeReader) GetGitChange(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	reader.changeReads++
	if reader.requireSnapshot && ctx.Value(statusFacadeSnapshotKey{}) != reader {
		return domain.GitChangeJob{}, domain.ErrIntegrity
	}
	return reader.job, nil
}

func (reader *statusFacadeReader) ListGitChanges(ctx context.Context, repo domain.ContentHash, cursor string, limit int) ([]domain.GitChangeSummary, error) {
	reader.changeReads++
	if reader.requireSnapshot && ctx.Value(statusFacadeSnapshotKey{}) != reader {
		return nil, domain.ErrIntegrity
	}
	return reader.summaries, nil
}

type statusFacadeSnapshots struct {
	*statusFacadeReader
	finishError error
	snapshots   int
}

func (reader *statusFacadeSnapshots) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	reader.snapshots++
	if err := read(context.WithValue(ctx, statusFacadeSnapshotKey{}, reader.statusFacadeReader)); err != nil {
		return err
	}
	return reader.finishError
}

type statusFacadeChangesOnly struct {
	reader *statusFacadeReader
}

func (reader statusFacadeChangesOnly) GetGitChange(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	return reader.reader.GetGitChange(ctx, repo, id)
}

func (reader statusFacadeChangesOnly) ListGitChanges(ctx context.Context, repo domain.ContentHash, cursor string, limit int) ([]domain.GitChangeSummary, error) {
	return reader.reader.ListGitChanges(ctx, repo, cursor, limit)
}

func TestGitChangeFacadeUsesStandaloneReadOnlyMetadataAndSnapshotPorts(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	reader := &statusFacadeSnapshots{statusFacadeReader: &statusFacadeReader{
		metadata: domain.Repo{ID: repo}, job: domain.GitChangeJob{ID: strings.Repeat("a", 64)}, requireSnapshot: true,
	}}
	queries := app.NewGitChangeQuery(reader)
	if _, standalone := queries.(*evidencequery.ChangeQueries); !standalone || reader.metadataReads != 0 || reader.changeReads != 0 || reader.snapshots != 0 {
		test.Fatal("facade retained Service or performed construction I/O", queries, reader)
	}
	view, err := queries.Get(context.Background(), repo, reader.job.ID)
	if err != nil || !reflect.DeepEqual(view, reader.job) || reader.snapshots != 1 || reader.metadataReads != 1 || reader.changeReads != 1 {
		test.Fatal("facade did not use captured read-only ports in one pinned scope", view, err, reader)
	}
	reader.finishError = domain.ErrConflict
	view, err = queries.Get(context.Background(), repo, reader.job.ID)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(view, domain.GitChangeJob{}) || reader.snapshots != 2 || reader.changeReads != 2 {
		test.Fatal("snapshot completion leaked change evidence", view, err, reader)
	}
}

func TestGitChangeFacadePreservesMissingMetadataAndRootOnlyLegacyMeaning(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	reader := &statusFacadeReader{metadata: domain.Repo{ID: repo}, job: domain.GitChangeJob{ID: strings.Repeat("a", 64)}}
	for _, queries := range []inbound.GitChangeQuery{app.NewGitChangeQuery(nil), app.NewGitChangeQuery(statusFacadeChangesOnly{reader: reader})} {
		view, err := queries.Get(context.Background(), domain.ContentHash("bad"), "bad")
		page, pageError := queries.List(context.Background(), domain.ContentHash("bad"), "bad", 0)
		if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !errors.Is(pageError, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(view, domain.GitChangeJob{}) || !reflect.DeepEqual(page, domain.GitChangePage{}) || reader.changeReads != 0 {
			test.Fatal("missing metadata lost identity-error precedence or reached reader", view, err, page, pageError)
		}
	}
	queries := app.NewGitChangeQuery(reader)
	reader.repoError = domain.ErrNotFound
	if view, err := queries.Get(context.Background(), repo, reader.job.ID); err != nil || view.ID != reader.job.ID || reader.changeReads != 1 {
		test.Fatal("missing legacy registration became an unconditional missing-repository error", view, err)
	}
	reader.repoError, reader.metadata.RequiredDocIdentity = nil, domain.DocumentIdentityRootV1
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1})
	if view, err := queries.Get(ctx, repo, "bad"); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || !reflect.DeepEqual(view, domain.GitChangeJob{}) || reader.changeReads != 1 {
		test.Fatal("metadata-only facade advertised root-document capability or changed gate precedence", view, err)
	}
}

func TestGitChangeFacadeRunsSnapshotAuthorizationBeforeReadingEvidence(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	reader := &statusFacadeSnapshots{statusFacadeReader: &statusFacadeReader{metadata: domain.Repo{ID: repo}, requireSnapshot: true}}
	queries := app.NewGitChangeQuery(reader)
	calls := 0
	ctx := inbound.WithDocumentSnapshotReadAuthorizer(context.Background(), func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		calls++
		if selected != repo || bound.Value(statusFacadeSnapshotKey{}) != reader.statusFacadeReader {
			test.Fatal("authorization escaped the pinned repository scope")
		}
		return bound, domain.ErrForbidden
	})
	page, err := queries.List(ctx, repo, "", 1)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.GitChangePage{}) || calls != 1 || reader.metadataReads != 0 || reader.changeReads != 0 {
		test.Fatal("revoked snapshot authority accessed metadata or change evidence", page, err, calls, reader)
	}
}
