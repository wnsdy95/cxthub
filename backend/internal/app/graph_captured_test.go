package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type capturedGraphEvidence struct {
	metadata       domain.Repo
	commits        map[string]domain.GitCommitTree
	checkpoint     domain.PRHistoryCheckpoint
	observations   map[string]domain.GitRefObservation
	participations []domain.PRParticipation
	prs            []domain.PRContribution
	git            []domain.GitContribution
	receipts       map[int64]domain.GitHeadReceipt
	admission      domain.GitContributionAdmission
	head           domain.GitContextHead
	proofs         []domain.GitContributionProof
	calls          int
	err            error
	duringRead     func()
}

var _ outbound.GraphEvidenceReader = (*capturedGraphEvidence)(nil)

func capturedGraphValue[Value any](ctx context.Context, reader *capturedGraphEvidence, repo domain.ContentHash, origin string, value Value, found bool) (Value, error) {
	var empty Value
	reader.calls++
	if reader.duringRead != nil {
		reader.duringRead()
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if repo != reader.metadata.ID || (origin != "" && origin != reader.metadata.GitRemoteURL) {
		return empty, domain.ErrIntegrity
	}
	if reader.err != nil {
		return empty, reader.err
	}
	if !found {
		return empty, domain.ErrNotFound
	}
	return value, nil
}

func (reader *capturedGraphEvidence) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.metadata, true)
}

func (reader *capturedGraphEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	value, found := reader.commits[commit]
	return capturedGraphValue(ctx, reader, repo, origin, value, found)
}

func (reader *capturedGraphEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, tree string) (domain.GitTreeNode, error) {
	return capturedGraphValue(ctx, reader, repo, origin, domain.GitTreeNode{}, false)
}

func (reader *capturedGraphEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return capturedGraphValue(ctx, reader, repo, origin, domain.GitCommitDelta{}, false)
}

func (reader *capturedGraphEvidence) GetPRHistoryCheckpoint(ctx context.Context, repo domain.ContentHash, origin string, provider int64, number int) (domain.PRHistoryCheckpoint, error) {
	return capturedGraphValue(ctx, reader, repo, origin, reader.checkpoint, reader.checkpoint.ProviderRepositoryID == provider && reader.checkpoint.PR.Number == number)
}

func (reader *capturedGraphEvidence) GetPRRefObservation(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitRefObservation, error) {
	value, found := reader.observations[id]
	return capturedGraphValue(ctx, reader, repo, origin, value, found)
}

func (reader *capturedGraphEvidence) ListPRParticipations(ctx context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	return capturedGraphValue(ctx, reader, repo, origin, reader.participations, reader.checkpoint.PR.Number == number)
}

func (reader *capturedGraphEvidence) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.prs, true)
}

func (reader *capturedGraphEvidence) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.git, true)
}

func (reader *capturedGraphEvidence) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	value, found := reader.receipts[version]
	return capturedGraphValue(ctx, reader, repo, origin, value, found)
}

func (reader *capturedGraphEvidence) GetGitContributionAdmission(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContributionAdmission, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.admission, reader.admission.ID == id)
}

func (reader *capturedGraphEvidence) GetGitContextHead(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContextHead, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.head, reader.head.ID == id)
}

func (reader *capturedGraphEvidence) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	return capturedGraphValue(ctx, reader, repo, "", reader.proofs, true)
}

type poisonedGraphMetadata struct{ outbound.MetadataStore }

func (*poisonedGraphMetadata) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	panic("projection read live repository metadata")
}

func (*poisonedGraphMetadata) InReadOnlyTransaction(context.Context) bool {
	panic("projection inspected live transaction state")
}

type capturedGraphRepositoryOnly struct {
	outbound.MetadataStore
	metadata domain.Repo
}

func (reader *capturedGraphRepositoryOnly) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return reader.metadata, nil
}

func TestGraphProjectionHelpersPreserveOptionalReaders(t *testing.T) {
	repo := hh(t.Name())
	metadata := domain.Repo{ID: repo}
	service := NewService(&capturedGraphRepositoryOnly{metadata: metadata}, nil, nil, nil, nil)
	ctx := context.Background()
	evidence, err := service.newCodeEvidence(ctx, repo)
	if err != nil || evidence.trees != nil || evidence.deltas != nil {
		t.Fatalf("missing optional adapters changed: %+v %v", evidence, err)
	}
	prs, err := service.verifiedPRContributions(ctx, repo, nil)
	if err != nil || len(prs) != 0 {
		t.Fatalf("missing PR adapter changed: %+v %v", prs, err)
	}
	git, err := service.queryGitContributions(ctx, repo, nil)
	if err != nil || len(git) != 0 {
		t.Fatalf("missing Git adapter changed: %+v %v", git, err)
	}
	history, err := service.readGitAdmissionHistory(ctx, repo, nil)
	if err != nil || history.repo.ID != repo {
		t.Fatalf("worker repository lookup changed: %+v %v", history, err)
	}
}

func TestCapturedGraphCodeEvidenceDoesNotFallBack(t *testing.T) {
	repo := hh(t.Name())
	reader := &capturedGraphEvidence{metadata: domain.Repo{ID: repo, GitRemoteURL: "https://github.com/example/captured"}}
	service := &Service{projectionEvidence: reader, meta: &poisonedGraphMetadata{}}
	ctx := context.Background()
	evidence, err := service.newCodeEvidence(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	if _, err := evidence.GetGitCommitTree(ctx, repo, reader.metadata.GitRemoteURL, commit); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing captured commit: %v", err)
	}
	if _, err := evidence.GetGitTreeNode(ctx, repo, reader.metadata.GitRemoteURL, commit); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing captured tree: %v", err)
	}
	if _, err := evidence.GetGitDelta(ctx, repo, reader.metadata.GitRemoteURL, commit, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing captured delta: %v", err)
	}
	calls := reader.calls
	if _, err := evidence.GetGitCommitTree(ctx, hh("foreign"), reader.metadata.GitRemoteURL, commit); !errors.Is(err, domain.ErrIntegrity) || reader.calls != calls {
		t.Fatalf("foreign repository reached evidence: %v", err)
	}
	if _, err := evidence.GetGitCommitTree(ctx, repo, "https://github.com/example/foreign", commit); !errors.Is(err, domain.ErrIntegrity) || reader.calls != calls {
		t.Fatalf("foreign origin reached evidence: %v", err)
	}
}

type capturedGraphMetadataReply struct {
	outbound.GraphEvidenceReader
	metadata domain.Repo
}

func (reader capturedGraphMetadataReply) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return reader.metadata, nil
}

func TestProjectCapturedGraphRejectsForeignMetadataReply(t *testing.T) {
	repo := hh(t.Name())
	reader := capturedGraphMetadataReply{metadata: domain.Repo{ID: hh("foreign")}}
	graph, err := ProjectCapturedGraph(context.Background(), testProjectionGeneration(repo), domain.RepositoryView{}, reader, "")
	if !errors.Is(err, domain.ErrIntegrity) || graph.Version != 0 {
		t.Fatalf("foreign metadata reply accepted: %+v %v", graph, err)
	}
}

func capturedPRGraphFixture(t *testing.T) (*capturedGraphEvidence, domain.RepositoryView, domain.GraphState) {
	t.Helper()
	fixture := newPRMemoryFixture(t)
	fixture.publish(t, "", true)
	fixture.reconcile(t)
	ctx := systemTestContext()
	view, err := fixture.worker.core.loadRepositoryView(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fixture.worker.core.projectGraphState(ctx, view, "")
	if err != nil {
		t.Fatal(err)
	}
	reader := &capturedGraphEvidence{commits: map[string]domain.GitCommitTree{}, observations: map[string]domain.GitRefObservation{}}
	reader.metadata, err = fixture.storage.GetRepo(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader.prs, err = fixture.storage.ListPRContributions(ctx, fixture.repo.ID)
	if err != nil || len(reader.prs) != 1 {
		t.Fatalf("missing PR fixture: %+v %v", reader.prs, err)
	}
	record := reader.prs[0]
	reader.checkpoint, err = fixture.storage.GetPRHistoryCheckpoint(ctx, record.RepoID, record.GitOrigin, record.ProviderRepositoryID, record.PR.Number)
	if err != nil {
		t.Fatal(err)
	}
	reader.participations, err = fixture.storage.ListPRParticipations(ctx, record.RepoID, record.GitOrigin, record.PR.Number)
	if err != nil || len(reader.participations) == 0 {
		t.Fatalf("missing participation fixture: %v", err)
	}
	for _, participation := range reader.participations {
		observation, err := fixture.storage.GetPRRefObservation(ctx, record.RepoID, record.GitOrigin, participation.ObservationID)
		if err != nil {
			t.Fatal(err)
		}
		reader.observations[observation.ID] = observation
	}
	var queue []string
	for _, event := range view.History {
		queue = append(queue, event.GitAfter)
	}
	queue = append(queue, record.PR.MergeSHA, record.OriginalHead)
	seen := map[string]bool{}
	for len(queue) > 0 {
		commit := queue[0]
		queue = queue[1:]
		if commit == "" || seen[commit] {
			continue
		}
		seen[commit] = true
		value, err := fixture.storage.GetGitCommitTree(ctx, record.RepoID, record.GitOrigin, commit)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		reader.commits[commit] = value
		queue = append(queue, value.Parents...)
	}
	return reader, view, want
}

func TestProjectCapturedGraphReadOnlyPRParity(t *testing.T) {
	reader, view, want := capturedPRGraphFixture(t)
	if _, broad := any(reader).(outbound.MetadataStore); broad {
		t.Fatal("test reader exposes mutable metadata")
	}
	checkpoint := testProjectionGeneration(reader.metadata.ID)
	view.Graph = &domain.GraphState{Version: -1}
	got, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, "")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("offline graph differs from direct service: %v\ngot %+v\nwant %+v", err, got, want)
	}
	merges := got.BranchContexts["main"].Merges
	if len(merges) != 1 || !merges[0].DeliveryAcknowledged || merges[0].State != "included" {
		t.Fatalf("read-only PR evidence did not verify: %+v", merges)
	}
	projector := &Service{projectionEvidence: reader, meta: &poisonedGraphMetadata{}}
	got, err = projector.projectGraphState(context.Background(), view, "")
	if err != nil || !reflect.DeepEqual(got, want) || projector.branchCache.Stats().Entries != 0 {
		t.Fatalf("isolated helpers used live metadata or cache: %v", err)
	}
	got.SnapshotIDs[0] = hh("caller mutation")
	again, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, "")
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("projection retained caller output: %v", err)
	}
	if len(want.Positions) == 0 {
		t.Fatal("fixture has no historical graph position")
	}
	position := want.Positions[0].EventID
	wantPosition, err := domain.ProjectGraphState(view, view.DefaultBranch, position)
	if err != nil {
		t.Fatal(err)
	}
	wantPosition.Integrations = []domain.GraphIntegration{}
	gotPosition, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, position)
	if err != nil || !reflect.DeepEqual(gotPosition, wantPosition) {
		t.Fatalf("historical position scope changed: %v", err)
	}
}

func TestProjectCapturedGraphDiscardsDerivedSemantics(t *testing.T) {
	reader, view, want := capturedPRGraphFixture(t)
	view.Semantics = domain.ContextSemantics{Version: 99}
	view.Graph = &domain.GraphState{Version: -1}
	got, err := ProjectCapturedGraph(context.Background(), testProjectionGeneration(reader.metadata.ID), view, reader, "")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("caller-derived semantics changed frozen projection: %v\ngot %+v\nwant %+v", err, got, want)
	}
	if view.Semantics.Version != 99 || view.Graph.Version != -1 {
		t.Fatal("projection mutated caller state")
	}
}

func TestProjectCapturedGraphMissingPREvidence(t *testing.T) {
	for _, missing := range []string{"checkpoint", "participation", "observation", "commit"} {
		t.Run(missing, func(t *testing.T) {
			reader, view, _ := capturedPRGraphFixture(t)
			switch missing {
			case "checkpoint":
				reader.checkpoint = domain.PRHistoryCheckpoint{}
			case "participation":
				reader.participations = nil
			case "observation":
				reader.observations = nil
			case "commit":
				reader.commits = nil
			}
			graph, err := ProjectCapturedGraph(context.Background(), testProjectionGeneration(reader.metadata.ID), view, reader, "")
			if missing == "observation" {
				if !errors.Is(err, domain.ErrNotFound) || graph.Version != 0 {
					t.Fatalf("missing proof observation was accepted: %+v %v", graph, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			merges := graph.BranchContexts["main"].Merges
			if len(merges) != 1 || merges[0].State != "review" || merges[0].DeliveryAcknowledged != (missing == "commit") {
				t.Fatalf("missing %s fabricated inclusion: %+v", missing, merges)
			}
		})
	}
}

func TestProjectCapturedGraphValidatesScopeBeforeReads(t *testing.T) {
	for _, target := range []string{"version", "scope", "epoch", "sequence", "repository", "ref", "snapshot", "history", "pending", "unsync", "archive"} {
		t.Run(target, func(t *testing.T) {
			repo, foreign := hh("captured repo"), hh("foreign repo")
			checkpoint := testProjectionGeneration(repo)
			view := domain.RepositoryView{}
			reader := &capturedGraphEvidence{metadata: domain.Repo{ID: repo}}
			switch target {
			case "version":
				checkpoint.Version++
			case "scope":
				checkpoint.Scope = domain.RepositoryViewSourceScope
			case "epoch":
				checkpoint.Epoch = ""
			case "sequence":
				checkpoint.Sequence = -1
			case "repository":
				checkpoint.RepoID = "bad"
			case "ref":
				view.Refs = []domain.Ref{{RepoID: foreign}}
			case "snapshot":
				view.Snapshots = []domain.Snapshot{{RepoID: foreign}}
			case "history":
				view.History = []domain.HistoryEvent{{RepoID: string(foreign)}}
			case "pending":
				view.Pending = []domain.Pending{{RepoID: foreign}}
			case "unsync":
				view.Unsync = []domain.Unsync{{RepoID: foreign}}
			case "archive":
				view.ArchivedSessions = []domain.SessionArchiveView{{SessionArchive: domain.SessionArchive{RepoID: foreign}}}
			}
			graph, err := ProjectCapturedGraph(context.Background(), checkpoint, view, reader, "historical")
			if err == nil || reader.calls != 0 || graph.Version != 0 {
				t.Fatalf("invalid %s reached evidence: calls=%d err=%v", target, reader.calls, err)
			}
		})
	}
}

func TestProjectCapturedGraphCancellationAndSourceErrors(t *testing.T) {
	repo := hh("captured repo")
	checkpoint := testProjectionGeneration(repo)
	for _, scenario := range []string{"nil evidence", "missing repository", "poisoned source", "metadata mismatch", "pre-canceled", "cancel during read", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &capturedGraphEvidence{metadata: domain.Repo{ID: repo}}
			var evidence outbound.GraphEvidenceReader = reader
			want := domain.ErrIntegrity
			switch scenario {
			case "nil evidence":
				evidence, want = nil, domain.ErrValidation
			case "missing repository":
				reader.err, want = domain.ErrNotFound, domain.ErrNotFound
			case "poisoned source":
				reader.err = want
			case "metadata mismatch":
				reader.metadata.DefaultBranch = "other"
			case "pre-canceled":
				cancel()
				want = context.Canceled
			case "cancel during read":
				reader.duringRead, want = cancel, context.Canceled
			case "empty":
				want = nil
			}
			graph, err := ProjectCapturedGraph(ctx, checkpoint, domain.RepositoryView{}, evidence, "")
			if !errors.Is(err, want) || (err != nil && graph.Version != 0) {
				t.Fatalf("unexpected projection result: %+v %v, want %v", graph, err, want)
			}
			if scenario == "pre-canceled" && reader.calls != 0 {
				t.Fatal("canceled projection read evidence")
			}
		})
	}
}

func capturedGitGraphFixture(t *testing.T) (*capturedGraphEvidence, domain.RepositoryView) {
	t.Helper()
	fixture, frozen, baseline, evidence := admissionPreparationFixture(t)
	ctx := systemTestContext()
	admission, err := prepareGitAdmission(ctx, frozen, baseline, evidence)
	if err != nil {
		t.Fatal(err)
	}
	later := baseline
	later.Version = admission.AfterReadVersion + 1
	later.Heads = []domain.GitHeadReceiptHead{{Ref: "refs/heads/main", Commit: strings.Repeat("e", 40)}}
	later = rebuildAdmissionReceipt(later)
	destination := admission.Destination
	destination.ReadVersion, destination.ReadReceiptID = later.Version, later.ID
	destination.ObservationID, destination.DestinationHead = later.Heads[0].ObservationID, later.Heads[0].Commit
	head := (domain.GitContextHead{Destination: destination.WithID(), AnchorID: admission.DestinationAnchorID, ContextTarget: fixture.main.Target}).WithID()
	record := (domain.GitContribution{
		RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, ProviderRepositoryID: later.Provider.RepositoryID,
		DestinationBranchID: fixture.main.BranchID, SourceBranchID: admission.SourceBranchID, Capture: admission.Capture,
		OriginalHead: admission.OriginalHead, IntegrationSHA: strings.Repeat("d", 40),
	}).WithID()
	proof := (domain.GitContributionProof{Contribution: record, AdmissionID: admission.ID, ContextHeadID: head.ID}).WithID()
	if !proof.MatchesInputs(admission, head, later) || !admission.MatchesBaseline(baseline) {
		t.Fatal("invalid Git fixture")
	}
	view, err := fixture.worker.core.loadRepositoryView(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := fixture.storage.GetRepo(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &capturedGraphEvidence{
		metadata: metadata, commits: evidence.trees.(*introductionTreeReader).commits,
		git: []domain.GitContribution{record}, proofs: []domain.GitContributionProof{proof}, admission: admission, head: head,
		receipts: map[int64]domain.GitHeadReceipt{baseline.Version: baseline, later.Version: later},
	}
	return reader, view
}

func TestProjectCapturedGraphVerifiesGitBaseline(t *testing.T) {
	for _, scenario := range []string{"complete", "missing baseline", "wrong baseline", "missing admission", "missing head", "missing later receipt", "foreign contribution"} {
		t.Run(scenario, func(t *testing.T) {
			reader, view := capturedGitGraphFixture(t)
			want := domain.ErrNotFound
			switch scenario {
			case "complete":
				want = nil
			case "missing baseline":
				delete(reader.receipts, reader.admission.Destination.ReadVersion)
			case "wrong baseline":
				reader.receipts[reader.admission.Destination.ReadVersion] = reader.receipts[reader.head.Destination.ReadVersion]
				want = domain.ErrIntegrity
			case "missing admission":
				reader.admission = domain.GitContributionAdmission{}
			case "missing head":
				reader.head = domain.GitContextHead{}
			case "missing later receipt":
				delete(reader.receipts, reader.head.Destination.ReadVersion)
			case "foreign contribution":
				reader.git[0].RepoID = hh("foreign")
				reader.git[0] = reader.git[0].WithID()
				want = domain.ErrIntegrity
			}
			graph, err := ProjectCapturedGraph(context.Background(), testProjectionGeneration(reader.metadata.ID), view, reader, "")
			if !errors.Is(err, want) || (err != nil && graph.Version != 0) {
				t.Fatalf("Git proof result: %v, want %v", err, want)
			}
			if want == nil {
				merges := graph.BranchContexts["main"].Merges
				if len(merges) != 1 || !merges[0].DeliveryAcknowledged || merges[0].State != "included" {
					t.Fatalf("read-only Git proof did not verify: %+v", merges)
				}
			}
		})
	}
}
