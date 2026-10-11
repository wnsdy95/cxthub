package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func participationFixture() (PRHistoryCheckpoint, GitRefObservation, HistoryEvent, []HistoryEvent) {
	repo := ContentHash("sha256:" + strings.Repeat("1", 64))
	checkpoint := PRHistoryCheckpoint{RepoID: repo, GitOrigin: "https://github.com/acme/api.git", InstallationID: 789, ProviderRepositoryID: 91, PR: PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)}, Version: 1, PullRequestID: 701, MergedAt: time.Unix(300, 0).UTC(), NextAttempt: time.Unix(400, 0).UTC()}
	observation := GitRefObservation{RepoID: repo, GitOrigin: checkpoint.GitOrigin, Source: "github-pr-reconciliation", Ref: "refs/heads/feature", Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40), Forced: true, Delivery: "rewrite", PullRequest: &GitPullRequestObservation{InstallationID: 789, RepositoryID: 91, ID: 701, Number: 7, Action: "force-pushed", HeadRepositoryID: 91, BaseRepositoryID: 91, BaseRef: "main", ProviderEventID: "rewrite-event", OccurredAt: time.Unix(200, 0).UTC()}}.WithID()
	capture := ContentHash("sha256:" + strings.Repeat("2", 64))
	ordinary := HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature-identity", Branch: "feature", Kind: "birth", Source: capture, Target: capture, GitAfter: observation.Before, CreatedAt: time.Unix(100, 0).UTC()}
	publication := ordinary
	publication.ID, publication.Kind = strings.Repeat("2", 32), "publish"
	publication.CreatedAt = time.Unix(500, 0).UTC()
	return checkpoint, observation, publication, []HistoryEvent{ordinary, publication}
}

func TestPRParticipationPreservesOriginalLatePublication(t *testing.T) {
	checkpoint, observation, publication, history := participationFixture()
	participation, err := VerifyPRParticipation(checkpoint, observation, publication, history)
	if err != nil {
		t.Fatal(err)
	}
	if participation.OriginalHead != observation.Before || participation.MergeSHA != checkpoint.PR.MergeSHA || participation.PublicationID != publication.ID || participation.Capture != publication.Target || participation.Validate() != nil {
		t.Fatalf("incorrect participation: %+v", participation)
	}
	if publication.GitAfter == checkpoint.PR.HeadSHA || publication.GitAfter != history[0].GitAfter {
		t.Fatal("original publication relabeled")
	}
	corrupted := participation
	corrupted.Capture = checkpoint.RepoID
	if corrupted.Validate() == nil {
		t.Fatal("modified proof retained a valid identity")
	}
	observation.PullRequest.ProviderEventID = "another-witness"
	observation.Delivery = "another-delivery"
	observation = observation.WithID()
	second, err := VerifyPRParticipation(checkpoint, observation, publication, history)
	if err != nil || second.ID == participation.ID {
		t.Fatalf("independent witness was overwritten: %v", err)
	}
}

func TestPRParticipationAcceptsVerifiedMergedWebhookFinalHead(t *testing.T) {
	checkpoint, observation, publication, history := participationFixture()
	observation.Source, observation.Before, observation.After, observation.Forced = "github-pull-request", "", checkpoint.PR.HeadSHA, false
	observation.PullRequest.Action, observation.PullRequest.MergeSHA = "closed", checkpoint.PR.MergeSHA
	observation.PullRequest.ProviderEventID, observation.PullRequest.OccurredAt = "", time.Time{}
	observation = observation.WithID()
	publication.GitAfter, history[0].GitAfter = checkpoint.PR.HeadSHA, checkpoint.PR.HeadSHA
	history[1] = publication
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); err != nil {
		t.Fatal(err)
	}
	observation.PullRequest.MergeSHA = strings.Repeat("e", 40)
	observation = observation.WithID()
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); !errors.Is(err, ErrPRSourcePending) {
		t.Fatalf("different merge accepted: %v", err)
	}
}

func TestPRParticipationRejectsUnrelatedOrUnverifiedEvidence(t *testing.T) {
	for _, scenario := range []string{"different-pr", "different-repository", "different-origin", "different-head", "wrong-branch", "post-merge-rewrite", "post-merge-capture", "missing-original-observation", "unsigned-hint", "webhook-without-time"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, observation, publication, history := participationFixture()
			switch scenario {
			case "different-pr":
				observation.PullRequest.ID++
			case "different-repository":
				checkpoint.ProviderRepositoryID++
			case "different-origin":
				checkpoint.GitOrigin = "https://github.com/acme/other"
			case "different-head":
				publication.GitAfter = strings.Repeat("e", 40)
			case "wrong-branch":
				publication.Branch = "reused"
			case "post-merge-rewrite":
				observation.PullRequest.OccurredAt = checkpoint.MergedAt.Add(time.Second)
			case "post-merge-capture":
				history[0].CreatedAt = checkpoint.MergedAt.Add(time.Second)
			case "missing-original-observation":
				history = history[1:]
			case "unsigned-hint":
				observation.Source, observation.PullRequest, observation.Delivery = "reconciliation", nil, ""
			case "webhook-without-time":
				observation.Source, observation.Forced, observation.PullRequest.Action = "github-pull-request", false, "synchronize"
				observation.PullRequest.OccurredAt = time.Time{}
			}
			observation = observation.WithID()
			if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); !errors.Is(err, ErrPRSourcePending) {
				t.Fatalf("unsafe association accepted: %v", err)
			}
		})
	}
}

func TestPRHistoryCheckpointValidation(t *testing.T) {
	checkpoint, _, _, _ := participationFixture()
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	checkpoint.Complete = true
	if checkpoint.Validate() == nil {
		t.Fatal("complete without checked time accepted")
	}
	checkpoint.HistoryCheckedAt = time.Now().UTC()
	checkpoint.Cursor = "unfinished"
	if checkpoint.Validate() == nil {
		t.Fatal("complete with unfinished cursor accepted")
	}
	checkpoint.Cursor = ""
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPRParticipationRejectsReusedBranchIdentityAtSameHead(t *testing.T) {
	checkpoint, observation, publication, history := participationFixture()
	archived := history[0]
	archived.ID, archived.Kind, archived.Source = strings.Repeat("3", 32), "archive", archived.Target
	archived.CreatedAt = time.Unix(110, 0).UTC()
	reused := history[0]
	reused.ID, reused.BranchID = strings.Repeat("4", 32), "new-identity"
	reused.CreatedAt = time.Unix(120, 0).UTC()
	newPublication := reused
	newPublication.ID, newPublication.Kind = strings.Repeat("5", 32), "publish"
	history = append(history, archived, reused)
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); !errors.Is(err, ErrConflict) {
		t.Fatalf("reuse without finalized publication certified old identity: %v", err)
	}
	history = append(history, newPublication)
	for _, candidate := range []HistoryEvent{publication, newPublication} {
		if _, err := VerifyPRParticipation(checkpoint, observation, candidate, history); !errors.Is(err, ErrConflict) {
			t.Fatalf("ambiguous context identity certified: %+v, %v", candidate, err)
		}
	}
}

func TestPRParticipationRequiresRecordedNativeAlias(t *testing.T) {
	checkpoint, observation, publication, history := participationFixture()
	for index := range history {
		history[index].Branch, history[index].LocalBranch = "context-feature", "feature"
		history[index].WorktreeID = strings.Repeat("a", 32)
	}
	publication = history[1]
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); !errors.Is(err, ErrPRSourcePending) {
		t.Fatalf("unrecorded alias certified: %v", err)
	}
	attached := history[0]
	attached.Kind, attached.ID = "attach", strings.Repeat("b", 32)
	attached.CreatedAt = history[0].CreatedAt.Add(-time.Second)
	history = append(history, attached)
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); err != nil {
		t.Fatal(err)
	}
	history[len(history)-1].CreatedAt = checkpoint.MergedAt.Add(time.Second)
	if _, err := VerifyPRParticipation(checkpoint, observation, publication, history); !errors.Is(err, ErrPRSourcePending) {
		t.Fatalf("late alias certified past capture: %v", err)
	}
}
