package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestFSPRJobAssociationBatch(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	ctx, checkpoint, proof := prHistoryFixture(t, storage)
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRParticipation(ctx, proof); err != nil {
		t.Fatal(err)
	}
	contribution := (domain.PRContribution{RepoID: proof.RepoID, GitOrigin: proof.GitOrigin,
		ProviderRepositoryID: proof.ProviderRepositoryID, PullRequestID: proof.PullRequestID, PR: checkpoint.PR,
		DestinationBranchID: "main-id", SourceBranchID: proof.BranchID, Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
	if err := storage.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	result, err := storage.ReadPRJobAssociationEvidence(ctx, checkpoint.RepoID, []int{checkpoint.PR.Number})
	if err != nil || !result.CheckpointsComplete || !result.ParticipationsComplete || !result.ContributionsComplete || !reflect.DeepEqual(result.Checkpoints, []domain.PRHistoryCheckpoint{checkpoint}) || !reflect.DeepEqual(result.Participations, []domain.PRParticipation{proof}) || !reflect.DeepEqual(result.Contributions, []domain.PRContribution{contribution}) {
		t.Fatal("batch mismatch", result, err)
	}
	for _, scope := range []struct {
		repo   domain.ContentHash
		number int
	}{{checkpoint.RepoID, checkpoint.PR.Number + 1}, {domain.HashContent([]byte("other repo")), checkpoint.PR.Number}} {
		result, err := storage.ReadPRJobAssociationEvidence(ctx, scope.repo, []int{scope.number})
		if err != nil || !result.ContributionsComplete || len(result.Checkpoints)+len(result.Participations)+len(result.Contributions) != 0 {
			t.Fatal("cross-scope batch", result, err)
		}
	}
	for _, numbers := range [][]int{nil, {0}, make([]int, 101)} {
		if _, err := storage.ReadPRJobAssociationEvidence(ctx, checkpoint.RepoID, numbers); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid batch accepted", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.ReadPRJobAssociationEvidence(canceled, checkpoint.RepoID, []int{checkpoint.PR.Number}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation swallowed", err)
	}
}

func TestFSPRJobAssociationCapsDirectoryBeforeLoadingPayloads(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	directory := filepath.Join(storage.dataDir, "pr-contributions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for index := range outbound.PRJobAssociationReadLimit {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%05d.skip", index)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	repo := domain.HashContent([]byte("bounded batch"))
	result, err := storage.ReadPRJobAssociationEvidence(context.Background(), repo, []int{1})
	if err != nil || !result.ContributionsComplete {
		t.Fatal("exact entry budget should be complete", result, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "overflow.json"), []byte("invalid private payload must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = storage.ReadPRJobAssociationEvidence(context.Background(), repo, []int{1})
	if err != nil || result.ContributionsComplete || len(result.Contributions) != 0 || !result.ParticipationsComplete || !result.CheckpointsComplete {
		t.Fatal("overflow must be unknown, not partial counts or a payload read", result, err)
	}
}
