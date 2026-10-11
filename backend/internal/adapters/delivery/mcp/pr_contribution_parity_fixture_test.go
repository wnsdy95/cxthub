//go:build postgres

package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	httpdelivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type prContributionParityFixture struct {
	ctx                       context.Context
	storage                   *store.PostgresStore
	service                   *app.Service
	httpHandler, mcpHandler   http.Handler
	httpToken, mcpToken       string
	outsiderHTTP, outsiderMCP string
	repo                      domain.Repo
	current                   domain.Snapshot
	observation, publication  domain.HistoryEvent
	contribution              domain.PRContribution
	archive                   domain.MemoryDigest
	archiveHash               domain.ContentHash
	code                      string
}

func newPRContributionParityFixture(t *testing.T) *prContributionParityFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	dsn := rootProjectionIsolatedDSN(t, ctx)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	system := inbound.WithSystemActor(ctx)
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	owner, ownerSession, err := identity.Login(ctx, "dev:parity-owner@example.test:Parity Owner", "parity test")
	if err != nil {
		t.Fatal(err)
	}
	outsider, outsiderSession, err := identity.Login(ctx, "dev:parity-outsider@example.test:Parity Outsider", "parity test")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(system, owner, "ContributionParity")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/example/contribution-parity.git"}
	if _, err := storage.PutRepo(system, repo); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	fixture := &prContributionParityFixture{ctx: system, storage: storage, service: service, repo: repo, httpToken: ownerSession.Token, outsiderHTTP: outsiderSession.Token, code: strings.Repeat("e", 40)}
	base := fixture.snapshot(t, "base", "main", time.Unix(50, 0))
	capture := fixture.snapshot(t, "historical contribution", "feature", time.Unix(100, 0), base.ID)
	fixture.current = fixture.snapshot(t, "newer main work", "main", time.Unix(400, 0), base.ID)
	if err := storage.CompareAndSwapRef(system, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: "main-context", Target: fixture.current.ID}, ""); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	merge := strings.Repeat("d", 40)
	root := strings.Repeat("1", 40)
	entry := domain.GitEntry{OID: strings.Repeat("f", 40), Mode: "100644"}
	fixture.gitCommit(t, root, "", nil, map[string]domain.GitEntry{})
	fixture.gitCommit(t, head, root, []domain.GitPathChange{{Path: "file", After: entry}}, map[string]domain.GitEntry{"file": entry})
	fixture.gitCommit(t, merge, root, []domain.GitPathChange{{Path: "file", After: entry}}, map[string]domain.GitEntry{"file": entry})
	fixture.gitCommit(t, fixture.code, merge, nil, map[string]domain.GitEntry{"file": entry})
	digest := func(previous domain.ContentHash, texts ...string) domain.MemoryDigest {
		claims := []domain.MemoryClaim{}
		for _, text := range texts {
			claims = append(claims, domain.MemoryClaim{Kind: "code", Text: text, Code: &domain.MemoryCodeScope{Commit: head, Paths: []string{"file"}}})
		}
		return domain.MemoryDigest{SnapshotID: capture.ID, PreviousMemoryHash: previous, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: capture.ID, Claims: claims}}}
	}
	fixture.archive = digest("", "removed archive claim", "retained historical claim")
	fixture.archiveHash, err = storage.PutMemory(system, repo.ID, fixture.archive)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := storage.PutMemory(system, repo.ID, digest(fixture.archiveHash, "retained historical claim"))
	if err != nil {
		t.Fatal(err)
	}
	mutable, err := storage.PutMemory(system, repo.ID, digest(pinned, "unpublished mutable claim"))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompareAndSwapSnapshotMemory(system, repo.ID, capture.ID, "", mutable); err != nil {
		t.Fatal(err)
	}
	fixture.observation = domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: "feature-context", Branch: "feature", Kind: "position", Source: capture.ID, Target: capture.ID, GitAfter: head, CreatedAt: time.Unix(100, 0).UTC()}
	fixture.publication = fixture.observation
	fixture.publication.ID, fixture.publication.Kind, fixture.publication.CreatedAt = strings.Repeat("2", 32), "publish", time.Unix(101, 0).UTC()
	fixture.publication.MemoryPinned, fixture.publication.MemorySource, fixture.publication.MemoryHash = true, capture.ID, pinned
	position := domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(repo.ID), BranchID: "main-context", Branch: "main", Kind: "position", Source: fixture.current.ID, Target: fixture.current.ID, GitAfter: fixture.code, CreatedAt: time.Unix(400, 0).UTC()}
	for _, event := range []domain.HistoryEvent{fixture.observation, fixture.publication, position} {
		if err := service.RecordHistory(system, event); err != nil {
			t.Fatal(err)
		}
	}
	pr := domain.PullRequestMerge{Number: 492, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: merge}
	checkpoint := domain.PRHistoryCheckpoint{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, InstallationID: 9, ProviderRepositoryID: 91, PullRequestID: 4921, PR: pr, Version: 1, MergedAt: time.Unix(300, 0).UTC(), Complete: true, HistoryCheckedAt: time.Now().UTC(), NextAttempt: time.Now().UTC()}
	witness := (domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "github-pr-reconciliation", Ref: "refs/heads/feature", Before: head, After: pr.HeadSHA, Forced: true, Delivery: "synthetic-parity-rewrite", PullRequest: &domain.GitPullRequestObservation{InstallationID: 9, RepositoryID: 91, ID: 4921, Number: pr.Number, Action: "force-pushed", HeadRepositoryID: 91, BaseRepositoryID: 91, BaseRef: "main", ProviderEventID: "synthetic-parity-event", OccurredAt: time.Unix(200, 0).UTC()}}).WithID()
	history, err := service.ListHistory(system, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := domain.VerifyPRParticipation(checkpoint, witness, fixture.publication, history)
	if err != nil {
		t.Fatal(err)
	}
	fixture.contribution = (domain.PRContribution{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, ProviderRepositoryID: 91, PullRequestID: 4921, PR: pr, DestinationBranchID: "main-context", SourceBranchID: proof.BranchID, Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
	pin := (domain.PRContributionMemory{RepoID: repo.ID, ContributionID: fixture.contribution.ID, PublicationID: fixture.publication.ID, MemoryHash: pinned, MemorySource: capture.ID, MemoryPinned: true}).WithID()
	if err := storage.WithinRepository(system, repo.ID, func(tx context.Context) error {
		if err := storage.SavePRHistoryCheckpoint(tx, checkpoint, 0); err != nil {
			return err
		}
		if err := storage.RecordGitRefObservation(tx, witness, time.Now().UTC()); err != nil {
			return err
		}
		if err := storage.PutPRParticipation(tx, proof); err != nil {
			return err
		}
		if err := storage.PutPRContribution(tx, fixture.contribution); err != nil {
			return err
		}
		if err := storage.PutPRContributionMemory(tx, pin); err != nil {
			return err
		}
		return storage.AdvanceEvidenceRevision(tx, repo.ID)
	}); err != nil {
		t.Fatal(err)
	}
	api := httpdelivery.NewServer(service, identity)
	api.SetEffectiveMemory(service)
	api.SetMemoryPositions(service)
	fixture.httpHandler = api.Handler()
	server, err := NewServer(service, identity, storage, "https://parity.example.test")
	if err != nil {
		t.Fatal(err)
	}
	server.SetEffectiveMemory(service)
	fixture.mcpHandler = server.Handler()
	for _, account := range []struct {
		user  domain.User
		token *string
	}{{owner, &fixture.mcpToken}, {outsider, &fixture.outsiderMCP}} {
		pair, err := identity.IssueMCPTokenPair(system, account.user.ID, "contribution-parity-client")
		if err != nil {
			t.Fatal(err)
		}
		*account.token = pair.AccessToken
	}
	return fixture
}

func (fixture *prContributionParityFixture) snapshot(t *testing.T, text, branch string, created time.Time, parents ...domain.ContentHash) domain.Snapshot {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := fixture.storage.PutDoc(fixture.ctx, fixture.repo.ID, doc); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: fixture.repo.ID, Branch: branch, Message: text, Parents: parents, CreatedAt: created.UTC(), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := fixture.storage.PutSnapshot(fixture.ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (fixture *prContributionParityFixture) gitCommit(t *testing.T, commit, parent string, changes []domain.GitPathChange, entries map[string]domain.GitEntry) {
	t.Helper()
	now := time.Now().UTC()
	job := domain.NewGitScan(fixture.repo.ID, fixture.repo.GitRemoteURL, commit, now)
	if err := fixture.storage.EnqueueGitScan(fixture.ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.storage.ClaimGitScan(fixture.ctx, fixture.repo.ID, now.Add(time.Second), time.Minute)
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim fixture commit %s: %+v %v", commit, claimed, err)
	}
	parents := []string{}
	queued := []domain.GitScanJob{}
	if parent != "" {
		parents = append(parents, parent)
		queued = append(queued, domain.NewGitScan(fixture.repo.ID, fixture.repo.GitRemoteURL, parent, now))
	}
	node, err := domain.NewGitTreeNode(entries, 40)
	if err != nil {
		t.Fatal(err)
	}
	tree := domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: commit, Tree: node.OID, Parents: parents}, Nodes: []domain.GitTreeNode{node}}
	claimed.State, claimed.Indexed, claimed.TreeIndexed, claimed.LeaseUntil = "completed", true, true, time.Time{}
	delta := domain.NewGitDelta(fixture.repo.ID, fixture.repo.GitRemoteURL, domain.GitCommitDelta{Commit: commit, Parent: parent, Parents: parents, Changes: changes, Complete: true})
	if err := fixture.storage.FinishGitScan(fixture.ctx, domain.GitScanFinish{Job: claimed, Tree: &tree, Deltas: []domain.GitDeltaRecord{delta}, Parents: queued}); err != nil {
		t.Fatal(err)
	}
}
