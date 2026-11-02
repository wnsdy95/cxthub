package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphSourceFixture struct {
	metadata       domain.Repo
	checkpoint     domain.RepositoryViewSourceCheckpoint
	commit         domain.GitCommitTree
	tree           domain.GitTreeNode
	delta          domain.GitDeltaRecord
	observation    domain.GitRefObservation
	prCheckpoint   domain.PRHistoryCheckpoint
	participation  domain.PRParticipation
	prContribution domain.PRContribution
	baseline       domain.GitHeadReceipt
	receipt        domain.GitHeadReceipt
	admission      domain.GitContributionAdmission
	head           domain.GitContextHead
	proof          domain.GitContributionProof
}

func newGraphSourceFixture(test testing.TB) graphSourceFixture {
	test.Helper()
	repo := domain.HashContent([]byte("frozen graph repo"))
	origin := "https://github.com/acme/frozen.git"
	metadata := domain.Repo{ID: repo, GitRemoteURL: origin, DefaultBranch: "main", Topics: []string{"frozen"}}
	provider := domain.GitProviderObservation{InstallationID: 17, RepositoryID: 29}
	commit := strings.Repeat("a", 40)
	parent := strings.Repeat("b", 40)
	source := (domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/feature", After: strings.Repeat("c", 40), Provider: &provider}).WithID()
	makeReceipt := func(version int64, branch, head string, extra ...domain.GitRefObservation) (domain.GitHeadReceipt, domain.GitDestinationCandidate) {
		observation := (domain.GitRefObservation{RepoID: repo, GitOrigin: origin, Source: "reconciliation", Ref: "refs/heads/" + branch, After: head, Provider: &provider}).WithID()
		claim := domain.GitHeadScan{RepoID: repo, GitOrigin: origin, Version: version, Page: 1, State: "running", LeaseUntil: time.Unix(500, 0).UTC()}
		receipt, err := domain.NewGitHeadReceipt(claim, append([]domain.GitRefObservation{observation}, extra...), false)
		if err != nil || receipt == nil {
			test.Fatal("receipt fixture", err)
		}
		candidate := (domain.GitDestinationCandidate{RepoID: repo, GitOrigin: origin, InstallationID: provider.InstallationID, ProviderRepositoryID: provider.RepositoryID,
			ObservationID: observation.ID, ReadReceiptID: receipt.ID, ReadVersion: version, DestinationBranchID: "main-incarnation", DestinationBranch: branch, DestinationHead: head}).WithID()
		return *receipt, candidate
	}
	baseline, destination := makeReceipt(4, "main", parent, source)
	admission := (domain.GitContributionAdmission{Destination: destination, SourceBranchID: "feature-incarnation", SourceBranch: "feature",
		SourceAnchorID: strings.Repeat("1", 32), DestinationAnchorID: strings.Repeat("2", 32), PublicationID: strings.Repeat("3", 32),
		Capture: domain.HashContent([]byte("capture")), OriginalHead: source.After, SourceHead: source.After, SourceObservationID: source.ID,
		OrdinaryObservationIDs: []string{strings.Repeat("4", 32)}, CarryObservationIDs: []string{strings.Repeat("5", 32)}, AfterReadVersion: 6}).WithID()
	receipt, later := makeReceipt(7, "renamed-main", commit)
	head := (domain.GitContextHead{Destination: later, AnchorID: admission.DestinationAnchorID, ContextTarget: domain.HashContent([]byte("destination context")), GraphRevision: 3, EvidenceRevision: 7}).WithID()
	contribution := (domain.GitContribution{RepoID: repo, GitOrigin: origin, ProviderRepositoryID: provider.RepositoryID, DestinationBranchID: destination.DestinationBranchID,
		SourceBranchID: admission.SourceBranchID, Capture: admission.Capture, OriginalHead: admission.OriginalHead, IntegrationSHA: commit}).WithID()
	proof := (domain.GitContributionProof{AdmissionID: admission.ID, ContextHeadID: head.ID, Contribution: contribution}).WithID()
	if !admission.MatchesBaseline(baseline) || !proof.MatchesInputs(admission, head, receipt) {
		test.Fatal("invalid proof fixture")
	}
	tree, err := domain.NewGitTreeNode(map[string]domain.GitEntry{"file": {OID: strings.Repeat("d", 40), Mode: "100644"}}, 40)
	if err != nil {
		test.Fatal(err)
	}
	delta := domain.NewGitDelta(repo, origin, domain.GitCommitDelta{Commit: commit, Parent: parent, Parents: []string{parent}, Complete: true,
		Changes: []domain.GitPathChange{{Path: "file", After: tree.Entries["file"]}}})
	pr := domain.PullRequestMerge{Number: 11, BaseBranch: "main", HeadBranch: "feature", HeadSHA: source.After, MergeSHA: commit}
	observation := (domain.GitRefObservation{Delivery: "delivery", RepoID: repo, GitOrigin: origin, Source: "github-pull-request", Ref: source.Ref, After: source.After,
		PullRequest: &domain.GitPullRequestObservation{InstallationID: provider.InstallationID, RepositoryID: provider.RepositoryID, ID: 100, Number: pr.Number,
			Action: "closed", HeadRepositoryID: provider.RepositoryID, BaseRepositoryID: provider.RepositoryID, BaseRef: pr.BaseBranch, MergeSHA: pr.MergeSHA}}).WithID()
	prCheckpoint := domain.PRHistoryCheckpoint{RepoID: repo, GitOrigin: origin, InstallationID: provider.InstallationID, ProviderRepositoryID: provider.RepositoryID,
		PR: pr, Version: 2, PullRequestID: 100, MergedAt: time.Unix(400, 0).UTC(), Complete: true, HistoryCheckedAt: time.Unix(450, 0).UTC(), NextAttempt: time.Unix(500, 0).UTC()}
	participation := (domain.PRParticipation{RepoID: repo, GitOrigin: origin, ProviderRepositoryID: provider.RepositoryID, PullRequestID: 100, Number: pr.Number,
		MergeSHA: pr.MergeSHA, PublicationID: admission.PublicationID, Capture: admission.Capture, BranchID: admission.SourceBranchID, OriginalHead: source.After, ObservationID: observation.ID}).WithID()
	prContribution := (domain.PRContribution{RepoID: repo, GitOrigin: origin, ProviderRepositoryID: provider.RepositoryID, PullRequestID: 100, PR: pr,
		DestinationBranchID: destination.DestinationBranchID, SourceBranchID: admission.SourceBranchID, Capture: admission.Capture, OriginalHead: source.After}).WithID()
	return graphSourceFixture{metadata: metadata, checkpoint: domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 20},
		commit: domain.GitCommitTree{Commit: commit, Tree: tree.OID, Parents: []string{parent}}, tree: tree, delta: delta, observation: observation,
		prCheckpoint: prCheckpoint, participation: participation, prContribution: prContribution, baseline: baseline, receipt: receipt, admission: admission, head: head, proof: proof}
}

func graphSourceTestEntry(test testing.TB, metadata domain.Repo, value interface{ Validate() error }) domain.RepositoryViewSourceEntry {
	test.Helper()
	if err := value.Validate(); err != nil {
		test.Fatalf("invalid %T fixture: %v", value, err)
	}
	envelope := map[string]any{"repo_id": metadata.ID, "payload": value}
	var kind, key string
	switch record := value.(type) {
	case domain.GitCommitTree:
		kind, key = "git_commit", graphSourceTuple(metadata.GitRemoteURL, record.Commit)
		envelope["origin"], envelope["commit_oid"] = metadata.GitRemoteURL, record.Commit
	case domain.GitTreeNode:
		kind, key = "git_tree", graphSourceTuple(metadata.GitRemoteURL, record.OID)
		envelope["origin"], envelope["oid"] = metadata.GitRemoteURL, record.OID
	case domain.GitDeltaRecord:
		kind, key = "git_delta", graphSourceTuple(record.GitOrigin, record.Delta.Commit, record.Delta.Parent)
		envelope["id"], envelope["origin"], envelope["commit_oid"], envelope["parent_oid"] = record.ID, record.GitOrigin, record.Delta.Commit, record.Delta.Parent
	case domain.GitRefObservation:
		kind, key, envelope["id"] = "git_observation", record.ID, record.ID
	case domain.PRHistoryCheckpoint:
		kind, key = "pr_checkpoint", graphSourceTuple(record.GitOrigin, strconv.FormatInt(record.ProviderRepositoryID, 10), strconv.Itoa(record.PR.Number))
		envelope["origin"], envelope["provider_repository_id"], envelope["number"], envelope["version"] = record.GitOrigin, record.ProviderRepositoryID, record.PR.Number, record.Version
	case domain.PRParticipation:
		kind, key = "pr_participation", record.ID
		envelope["id"], envelope["origin"], envelope["provider_repository_id"], envelope["number"] = record.ID, record.GitOrigin, record.ProviderRepositoryID, record.Number
	case domain.PRContribution:
		kind, key, envelope["id"] = "pr_contribution", record.ID, record.ID
	case domain.GitContribution:
		kind, key, envelope["id"] = "git_contribution", record.ID, record.ID
	case domain.GitHeadReceipt:
		kind, key = "git_receipt", graphSourceTuple(record.GitOrigin, strconv.FormatInt(record.Version, 10))
		envelope["id"], envelope["origin"], envelope["version"] = record.ID, record.GitOrigin, record.Version
	case domain.GitContributionAdmission:
		kind, key = "git_admission", record.ID
		envelope["id"], envelope["admission_key"], envelope["origin"], envelope["receipt_id"] = record.ID, record.Key(), record.Destination.GitOrigin, record.Destination.ReadReceiptID
	case domain.GitContextHead:
		kind, key = "git_context_head", record.ID
		envelope["id"], envelope["origin"], envelope["receipt_id"] = record.ID, record.Destination.GitOrigin, record.Destination.ReadReceiptID
		envelope["destination_branch_id"], envelope["destination_branch"], envelope["read_version"] = record.Destination.DestinationBranchID, record.Destination.DestinationBranch, record.Destination.ReadVersion
	case domain.GitContributionProof:
		kind, key = "git_contribution_proof", record.ID
		envelope["id"], envelope["admission_id"], envelope["context_head_id"], envelope["contribution_id"] = record.ID, record.AdmissionID, record.ContextHeadID, record.Contribution.ID
	default:
		test.Fatalf("unsupported fixture %T", value)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		test.Fatal(err)
	}
	return domain.RepositoryViewSourceEntry{Sequence: 10, Kind: kind, Key: key, Value: raw}
}

func (fixture graphSourceFixture) entries(test testing.TB) []domain.RepositoryViewSourceEntry {
	test.Helper()
	entries := []domain.RepositoryViewSourceEntry{}
	for _, value := range []interface{ Validate() error }{fixture.commit, fixture.tree, fixture.delta, fixture.observation, fixture.prCheckpoint, fixture.participation,
		fixture.prContribution, fixture.baseline, fixture.receipt, fixture.admission, fixture.head, fixture.proof.Contribution, fixture.proof} {
		entries = append(entries, graphSourceTestEntry(test, fixture.metadata, value))
	}
	return entries
}

func graphSourceTestEqual[Value any](test *testing.T, got Value, err error, want Value) {
	test.Helper()
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatalf("read %T = %#v, %v; want %#v", got, got, err, want)
	}
}

func TestGraphSourceReaderAllKindsAndOwnership(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	entries := fixture.entries(test)
	ctx, repo, origin := context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL
	reader, err := NewGraphSourceReader(ctx, fixture.checkpoint, fixture.metadata, entries)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		clear(entry.Value)
	}
	fixture.metadata.Topics[0] = "caller mutation"
	for range 2 {
		metadata, err := reader.GetRepo(ctx, repo)
		wantMetadata := fixture.metadata
		wantMetadata.Topics = []string{"frozen"}
		graphSourceTestEqual(test, metadata, err, wantMetadata)
		metadata.Topics[0] = "returned mutation"
		commit, err := reader.GetGitCommitTree(ctx, repo, origin, fixture.commit.Commit)
		graphSourceTestEqual(test, commit, err, fixture.commit)
		commit.Parents[0] = "mutation"
		tree, err := reader.GetGitTreeNode(ctx, repo, origin, fixture.tree.OID)
		graphSourceTestEqual(test, tree, err, fixture.tree)
		delete(tree.Entries, "file")
		delta, err := reader.GetGitDelta(ctx, repo, origin, fixture.delta.Delta.Commit, fixture.delta.Delta.Parent)
		graphSourceTestEqual(test, delta, err, fixture.delta.Delta)
		delta.Parents[0], delta.Changes[0].Path = "mutation", "mutation"
		checkpoint, err := reader.GetPRHistoryCheckpoint(ctx, repo, origin, fixture.prCheckpoint.ProviderRepositoryID, fixture.prCheckpoint.PR.Number)
		graphSourceTestEqual(test, checkpoint, err, fixture.prCheckpoint)
		observation, err := reader.GetPRRefObservation(ctx, repo, origin, fixture.observation.ID)
		graphSourceTestEqual(test, observation, err, fixture.observation)
		observation.PullRequest.Number = 99
		participations, err := reader.ListPRParticipations(ctx, repo, origin, fixture.participation.Number)
		graphSourceTestEqual(test, participations, err, []domain.PRParticipation{fixture.participation})
		participations[0].ID = "mutation"
		prContributions, err := reader.ListPRContributions(ctx, repo)
		graphSourceTestEqual(test, prContributions, err, []domain.PRContribution{fixture.prContribution})
		prContributions[0].ID = "mutation"
		contributions, err := reader.ListGitContributions(ctx, repo)
		graphSourceTestEqual(test, contributions, err, []domain.GitContribution{fixture.proof.Contribution})
		contributions[0].ID = "mutation"
		receipt, err := reader.GetGitHeadReceipt(ctx, repo, origin, fixture.receipt.Version)
		graphSourceTestEqual(test, receipt, err, fixture.receipt)
		receipt.Heads[0].Commit = "mutation"
		admission, err := reader.GetGitContributionAdmission(ctx, repo, fixture.admission.ID)
		graphSourceTestEqual(test, admission, err, fixture.admission)
		admission.OrdinaryObservationIDs[0], admission.CarryObservationIDs[0] = "mutation", "mutation"
		head, err := reader.GetGitContextHead(ctx, repo, fixture.head.ID)
		graphSourceTestEqual(test, head, err, fixture.head)
		proofs, err := reader.ListGitContributionProofs(ctx, repo)
		graphSourceTestEqual(test, proofs, err, []domain.GitContributionProof{fixture.proof})
		proofs[0].Contribution.ID = "mutation"
	}
}

func TestGraphSourceReaderRejectsInvalidEnvelopesAndPayloads(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	for index, entry := range fixture.entries(test) {
		test.Run(entry.Kind+"/"+strconv.Itoa(index), func(test *testing.T) {
			for _, mutation := range []string{"repo", "key", "unknown-envelope", "unknown-payload", "null-payload", "duplicate-field", "deleted", "negative-sequence", "future-sequence", "missing-field", "null-field", "unknown-kind", "duplicate-row"} {
				test.Run(mutation, func(test *testing.T) {
					entries := fixture.entries(test)
					changed := &entries[index]
					var envelope map[string]json.RawMessage
					if err := json.Unmarshal(changed.Value, &envelope); err != nil {
						test.Fatal(err)
					}
					switch mutation {
					case "repo":
						envelope["repo_id"], _ = json.Marshal(domain.HashContent([]byte("other repo")))
					case "key":
						changed.Key = "wrong"
					case "unknown-envelope":
						envelope["secret"] = json.RawMessage(`"token"`)
					case "unknown-payload":
						envelope["payload"] = append([]byte(`{"secret":"token",`), envelope["payload"][1:]...)
					case "null-payload":
						envelope["payload"] = json.RawMessage(`null`)
					case "deleted":
						changed.Deleted = true
					case "negative-sequence":
						changed.Sequence = -1
					case "future-sequence":
						changed.Sequence = fixture.checkpoint.Sequence + 1
					case "missing-field":
						delete(envelope, "repo_id")
					case "null-field":
						envelope["repo_id"] = json.RawMessage(`null`)
					case "unknown-kind":
						changed.Kind = "snapshot"
					}
					changed.Value, _ = json.Marshal(envelope)
					if mutation == "duplicate-field" {
						changed.Value = append([]byte(`{"repo_id":`+string(envelope["repo_id"])+`,`), changed.Value[1:]...)
					}
					if mutation == "duplicate-row" {
						duplicate := *changed
						if strings.HasPrefix(duplicate.Key, "[") {
							duplicate.Key = strings.ReplaceAll(duplicate.Key, ",", ", ")
						}
						entries = append(entries, duplicate)
					}
					if reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); reader != nil || !errors.Is(err, domain.ErrIntegrity) {
						test.Fatalf("invalid source accepted: %v", err)
					}
				})
			}
			var envelope map[string]json.RawMessage
			_ = json.Unmarshal(entry.Value, &envelope)
			for field := range envelope {
				if field == "repo_id" || field == "payload" {
					continue
				}
				test.Run("mismatch-"+field, func(test *testing.T) {
					entries := fixture.entries(test)
					var changed map[string]json.RawMessage
					_ = json.Unmarshal(entries[index].Value, &changed)
					if bytes.HasPrefix(changed[field], []byte(`"`)) {
						changed[field] = json.RawMessage(`"mismatch"`)
					} else {
						changed[field] = json.RawMessage(`123456`)
					}
					entries[index].Value, _ = json.Marshal(changed)
					if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); !errors.Is(err, domain.ErrIntegrity) {
						test.Fatal("mismatched envelope accepted", err)
					}
				})
			}
		})
	}
}

func TestGraphSourceReaderRejectsEveryMissingJoin(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	for _, missing := range []struct{ kind, key string }{
		{"git_admission", fixture.admission.ID},
		{"git_receipt", graphSourceTuple(fixture.metadata.GitRemoteURL, strconv.FormatInt(fixture.baseline.Version, 10))},
		{"git_context_head", fixture.head.ID},
		{"git_receipt", graphSourceTuple(fixture.metadata.GitRemoteURL, strconv.FormatInt(fixture.receipt.Version, 10))},
		{"git_contribution", fixture.proof.Contribution.ID},
	} {
		test.Run(missing.kind+missing.key, func(test *testing.T) {
			entries := slices.DeleteFunc(fixture.entries(test), func(entry domain.RepositoryViewSourceEntry) bool {
				return entry.Kind == missing.kind && entry.Key == missing.key
			})
			if reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); reader != nil || !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("missing joined row silently omitted: %v", err)
			}
		})
	}
}

func TestGraphSourceReaderRejectsMismatchedJoins(test *testing.T) {
	for _, scenario := range []string{"baseline", "head-receipt", "proof-inputs"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphSourceFixture(test)
			switch scenario {
			case "baseline":
				fixture.admission.SourceHead = strings.Repeat("e", 40)
				source := (domain.GitRefObservation{RepoID: fixture.metadata.ID, GitOrigin: fixture.metadata.GitRemoteURL, Source: "reconciliation", Ref: "refs/heads/feature", After: fixture.admission.SourceHead, Provider: &fixture.baseline.Provider}).WithID()
				fixture.admission.SourceObservationID = source.ID
				fixture.admission = fixture.admission.WithID()
				fixture.proof.AdmissionID = fixture.admission.ID
			case "head-receipt":
				fixture.head.Destination.DestinationHead = strings.Repeat("e", 40)
				fixture.head.Destination = fixture.head.Destination.WithID()
				fixture.head = fixture.head.WithID()
				fixture.proof.ContextHeadID = fixture.head.ID
			case "proof-inputs":
				fixture.proof.Contribution.Capture = domain.HashContent([]byte("unrelated capture"))
				fixture.proof.Contribution = fixture.proof.Contribution.WithID()
			}
			fixture.proof = fixture.proof.WithID()
			if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, fixture.entries(test)); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("mismatched joined evidence accepted", err)
			}
		})
	}
}
