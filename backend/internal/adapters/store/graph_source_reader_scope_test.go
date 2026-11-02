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

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphSourceReaderCancellationAndScope(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, fixture.entries(test))
	if err != nil {
		test.Fatal(err)
	}
	origin := fixture.metadata.GitRemoteURL
	reads := []func(context.Context, domain.ContentHash) error{
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetRepo(ctx, repo)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitCommitTree(ctx, repo, origin, fixture.commit.Commit)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitTreeNode(ctx, repo, origin, fixture.tree.OID)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitDelta(ctx, repo, origin, fixture.commit.Commit, "")
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetPRHistoryCheckpoint(ctx, repo, origin, 29, 11)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetPRRefObservation(ctx, repo, origin, fixture.observation.ID)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.ListPRParticipations(ctx, repo, origin, 11)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.ListPRContributions(ctx, repo)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.ListGitContributions(ctx, repo)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitHeadReceipt(ctx, repo, origin, fixture.receipt.Version)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitContributionAdmission(ctx, repo, fixture.admission.ID)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.GetGitContextHead(ctx, repo, fixture.head.ID)
			return err
		},
		func(ctx context.Context, repo domain.ContentHash) error {
			_, err := reader.ListGitContributionProofs(ctx, repo)
			return err
		},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for index, read := range reads {
		for _, scenario := range []struct {
			ctx  context.Context
			repo domain.ContentHash
			want error
		}{
			{canceled, fixture.metadata.ID, context.Canceled},
			{context.Background(), "invalid", domain.ErrValidation},
			{context.Background(), domain.HashContent([]byte("other")), domain.ErrNotFound},
		} {
			if err := read(scenario.ctx, scenario.repo); !errors.Is(err, scenario.want) {
				test.Fatalf("read %d scope: %v, want %v", index, err, scenario.want)
			}
		}
	}
	if _, err := NewGraphSourceReader(canceled, fixture.checkpoint, fixture.metadata, fixture.entries(test)); !errors.Is(err, context.Canceled) {
		test.Fatal("constructor ignored cancellation", err)
	}
	if reflect.TypeOf(reader).NumMethod() != len(reads) {
		test.Fatal("frozen adapter exposes methods beyond its thirteen read operations")
	}
}

func TestGraphSourceReaderRejectsInvalidLookupArguments(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	ctx, repo, origin := context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL
	reader, err := NewGraphSourceReader(ctx, fixture.checkpoint, fixture.metadata, nil)
	if err != nil {
		test.Fatal(err)
	}
	invalid := []func() error{
		func() error { _, err := reader.GetGitCommitTree(ctx, repo, origin, "HEAD"); return err },
		func() error { _, err := reader.GetGitTreeNode(ctx, repo, origin, "HEAD"); return err },
		func() error {
			_, err := reader.GetGitDelta(ctx, repo, origin, fixture.commit.Commit, "HEAD")
			return err
		},
		func() error { _, err := reader.GetGitDelta(ctx, repo, "", fixture.commit.Commit, ""); return err },
		func() error { _, err := reader.GetPRHistoryCheckpoint(ctx, repo, origin, 0, 11); return err },
		func() error { _, err := reader.GetPRHistoryCheckpoint(ctx, repo, origin, 29, 0); return err },
		func() error { _, err := reader.GetPRRefObservation(ctx, repo, origin, "bad"); return err },
		func() error { _, err := reader.ListPRParticipations(ctx, repo, "", 11); return err },
		func() error { _, err := reader.ListPRParticipations(ctx, repo, origin, -1); return err },
		func() error { _, err := reader.GetGitHeadReceipt(ctx, repo, origin, 0); return err },
		func() error { _, err := reader.GetGitContributionAdmission(ctx, repo, "bad"); return err },
		func() error { _, err := reader.GetGitContextHead(ctx, repo, "bad"); return err },
	}
	for index, read := range invalid {
		if err := read(); !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("invalid lookup %d: %v", index, err)
		}
	}
	proofs, err := reader.ListGitContributionProofs(ctx, repo)
	graphSourceTestEqual(test, proofs, err, []domain.GitContributionProof{})
	if _, err := reader.GetGitContextHead(ctx, repo, fixture.head.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("missing head did not remain missing", err)
	}
}

func TestGraphSourceReaderCheckpointAndBounds(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	for _, scenario := range []struct {
		name   string
		mutate func(*domain.RepositoryViewSourceCheckpoint)
	}{
		{"version", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Version++ }},
		{"scope", func(checkpoint *domain.RepositoryViewSourceCheckpoint) {
			checkpoint.Scope = domain.RepositoryViewSourceScope
		}},
		{"epoch", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Epoch = "invalid" }},
		{"sequence", func(checkpoint *domain.RepositoryViewSourceCheckpoint) { checkpoint.Sequence = -1 }},
		{"repo", func(checkpoint *domain.RepositoryViewSourceCheckpoint) {
			checkpoint.RepoID = domain.HashContent([]byte("different"))
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			checkpoint := fixture.checkpoint
			scenario.mutate(&checkpoint)
			if reader, err := NewGraphSourceReader(context.Background(), checkpoint, fixture.metadata, nil); reader != nil || err == nil {
				test.Fatal("invalid checkpoint accepted")
			}
		})
	}
	entry := graphSourceTestEntry(test, fixture.metadata, fixture.commit)
	for _, entries := range [][]domain.RepositoryViewSourceEntry{
		make([]domain.RepositoryViewSourceEntry, domain.MaxPendingProjectionInputRows+1),
		{{Kind: entry.Kind, Key: strings.Repeat("k", domain.MaxRepositoryViewSourceKeyBytes+1), Value: entry.Value}},
		{{Kind: entry.Kind, Key: entry.Key, Value: bytes.Repeat([]byte(" "), domain.MaxRepositoryViewSourceImageBytes+1)}},
		func() []domain.RepositoryViewSourceEntry {
			result := make([]domain.RepositoryViewSourceEntry, 33)
			for index := range result {
				result[index] = domain.RepositoryViewSourceEntry{Kind: entry.Kind, Key: entry.Key, Value: bytes.Repeat([]byte(" "), domain.MaxRepositoryViewSourceImageBytes)}
			}
			return result
		}(),
	} {
		if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
			test.Fatal("unbounded input accepted", err)
		}
	}
	metadata := fixture.metadata
	metadata.Description = strings.Repeat("x", domain.MaxPendingProjectionInputBytes+1)
	if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, metadata, nil); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		test.Fatal("unbounded metadata accepted", err)
	}
	entry.Sequence = fixture.checkpoint.Sequence
	if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, []domain.RepositoryViewSourceEntry{entry}); err != nil {
		test.Fatal("checkpoint boundary rejected", err)
	}
	entry.Sequence = 0
	if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, []domain.RepositoryViewSourceEntry{entry}); err != nil {
		test.Fatal("baseline sequence zero rejected", err)
	}
}

func TestGraphSourceReaderOrigins(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	for _, origin := range []string{
		"https://github.com/acme/historical.git", "git@github.com:acme/historical.git", "ssh://github.com/acme/historical.git", "git://github.com/acme/historical.git", "git+ssh://github.com/acme/historical.git",
	} {
		test.Run(origin, func(test *testing.T) {
			contribution := fixture.prContribution
			contribution.GitOrigin = origin
			contribution = contribution.WithID()
			participation := fixture.participation
			participation.GitOrigin = origin
			participation = participation.WithID()
			observation := fixture.observation
			observation.GitOrigin = origin
			observation = observation.WithID()
			entries := append(fixture.entries(test), graphSourceTestEntry(test, fixture.metadata, contribution), graphSourceTestEntry(test, fixture.metadata, participation), graphSourceTestEntry(test, fixture.metadata, observation))
			reader, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries)
			if err != nil {
				test.Fatal("historical origin rejected", err)
			}
			got, err := reader.ListPRContributions(context.Background(), fixture.metadata.ID)
			want := []domain.PRContribution{fixture.prContribution, contribution}
			slices.SortFunc(want, func(left, right domain.PRContribution) int { return strings.Compare(left.ID, right.ID) })
			graphSourceTestEqual(test, got, err, want)
			participations, err := reader.ListPRParticipations(context.Background(), fixture.metadata.ID, origin, participation.Number)
			graphSourceTestEqual(test, participations, err, []domain.PRParticipation{participation})
			observed, err := reader.GetPRRefObservation(context.Background(), fixture.metadata.ID, origin, observation.ID)
			graphSourceTestEqual(test, observed, err, observation)
			if _, err := reader.GetPRRefObservation(context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL, observation.ID); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("observation lookup lost origin binding", err)
			}
		})
	}
	for _, origin := range []string{
		"/Users/private/repo", "file:///Users/private/repo", "ftp://github.com/acme/repo", "https://token:secret@github.com/acme/repo", "https://github.com/acme/repo?token=secret", "https://github.com/acme/repo#fragment",
		"token@github.com:acme/repo", "git@github.com:acme/repo?token=secret", "https://:443/repo", " https://github.com/acme/repo", "https://github.com/acme/repo\n",
	} {
		test.Run("unsupported/"+origin, func(test *testing.T) {
			for _, field := range []string{"RemoteURL", "GitRemoteURL", "LocalPath"} {
				metadata := fixture.metadata
				reflect.ValueOf(&metadata).Elem().FieldByName(field).SetString(origin)
				if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, metadata, nil); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("unsafe metadata accepted", field, err)
				}
			}
			contribution := fixture.prContribution
			contribution.GitOrigin = origin
			contribution = contribution.WithID()
			envelope := map[string]any{"repo_id": fixture.metadata.ID, "id": contribution.ID, "payload": contribution}
			raw, _ := json.Marshal(envelope)
			entry := domain.RepositoryViewSourceEntry{Kind: "pr_contribution", Key: contribution.ID, Value: raw}
			if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, []domain.RepositoryViewSourceEntry{entry}); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("unsafe identity-hashed evidence accepted", err)
			}
		})
	}
}

func TestGraphSourceReaderDeltaParentSelection(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	ctx, repo, origin := context.Background(), fixture.metadata.ID, fixture.metadata.GitRemoteURL
	for _, parentCount := range []int{0, 1, 2} {
		test.Run(strconv.Itoa(parentCount), func(test *testing.T) {
			delta := fixture.delta.Delta
			delta.Parents = []string{}
			for index := range parentCount {
				delta.Parents = append(delta.Parents, strings.Repeat(strconv.Itoa(index+1), 40))
			}
			parents := delta.Parents
			if len(parents) == 0 {
				parents = []string{""}
			}
			entries := []domain.RepositoryViewSourceEntry{}
			for _, parent := range parents {
				delta.Parent = parent
				entries = append(entries, graphSourceTestEntry(test, fixture.metadata, domain.NewGitDelta(repo, origin, delta)))
			}
			reader, err := NewGraphSourceReader(ctx, fixture.checkpoint, fixture.metadata, entries)
			if err != nil {
				test.Fatal(err)
			}
			got, err := reader.GetGitDelta(ctx, repo, origin, delta.Commit, "")
			if parentCount == 2 {
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("ambiguous comparison selected", got, err)
				}
			} else {
				graphSourceTestEqual(test, got, err, delta)
			}
			for _, parent := range parents {
				delta.Parent = parent
				got, err := reader.GetGitDelta(ctx, repo, origin, delta.Commit, parent)
				graphSourceTestEqual(test, got, err, delta)
			}
		})
	}
}

func TestGraphSourceReaderRejectsUniqueIndexCollisions(test *testing.T) {
	for _, scenario := range []string{"receipt", "admission", "head", "proof"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGraphSourceFixture(test)
			var extra interface{ Validate() error }
			switch scenario {
			case "receipt":
				receipt := fixture.receipt
				receipt.More = !receipt.More
				extra = receipt.WithID()
			case "admission":
				admission := fixture.admission
				admission.GraphRevision++
				extra = admission.WithID()
			case "head":
				head := fixture.head
				head.ContextTarget = domain.HashContent([]byte("collision"))
				extra = head.WithID()
			case "proof":
				proof := fixture.proof
				proof.Contribution.IntegrationSHA = strings.Repeat("e", 40)
				proof.Contribution = proof.Contribution.WithID()
				extra = proof.WithID()
			}
			entries := append(fixture.entries(test), graphSourceTestEntry(test, fixture.metadata, extra))
			if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("unique tuple collision accepted", err)
			}
		})
	}
}

func TestGraphSourceReaderNestedPayloadAllowlist(test *testing.T) {
	fixture := newGraphSourceFixture(test)
	for _, scenario := range []struct {
		kind string
		path []string
	}{
		{"git_tree", []string{"entries", "file"}},
		{"git_delta", []string{"delta"}},
		{"git_observation", []string{"pull_request"}},
		{"pr_checkpoint", []string{"pr"}},
		{"pr_contribution", []string{"pr"}},
		{"git_receipt", []string{"provider"}},
		{"git_admission", []string{"destination"}},
		{"git_context_head", []string{"destination"}},
		{"git_contribution_proof", []string{"contribution"}},
	} {
		test.Run(scenario.kind, func(test *testing.T) {
			entries := fixture.entries(test)
			for index := range entries {
				if entries[index].Kind != scenario.kind {
					continue
				}
				var envelope map[string]any
				_ = json.Unmarshal(entries[index].Value, &envelope)
				nested := envelope["payload"].(map[string]any)
				for _, key := range scenario.path {
					nested = nested[key].(map[string]any)
				}
				nested["credential"] = "secret"
				entries[index].Value, _ = json.Marshal(envelope)
				break
			}
			if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, entries); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("unknown nested field accepted", err)
			}
		})
	}
	for _, payload := range []string{
		`{"commit":"` + fixture.commit.Commit + `","commit":"` + fixture.commit.Commit + `","tree":"` + fixture.tree.OID + `","parents":[]}`,
		strings.Repeat(`{"nested":`, 66) + `{}` + strings.Repeat(`}`, 66),
	} {
		entry := graphSourceTestEntry(test, fixture.metadata, fixture.commit)
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal(entry.Value, &envelope)
		envelope["payload"] = json.RawMessage(payload)
		entry.Value, _ = json.Marshal(envelope)
		if _, err := NewGraphSourceReader(context.Background(), fixture.checkpoint, fixture.metadata, []domain.RepositoryViewSourceEntry{entry}); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("duplicate/deep nested payload accepted", err)
		}
	}
}
