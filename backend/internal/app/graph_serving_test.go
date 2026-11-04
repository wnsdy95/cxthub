package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphServingUsesRetainedProofNotLatestQualification(test *testing.T) {
	service, storage, repo, want := newGraphProjectionRead(test)
	storage.result.Qualification = &domain.GraphQualificationResult{GraphQualificationKey: storage.result.QualificationKey(),
		Expected: storage.qualificationDigest, Actual: storage.qualificationDigest}
	storage.result.Source.Sequence++
	storage.result.Ready = false
	storage.qualificationErr = errors.New("latest candidate not qualified")
	storage.qualificationMismatch = true
	view, err := service.GetRepositoryView(systemTestContext(), repo)
	if err != nil || view.ReadGeneration == nil || view.ReadGeneration.Current || !view.ReadGeneration.Complete {
		test.Fatal("retained qualified view unavailable", view.ReadGeneration, err)
	}
	generation := *view.ReadGeneration
	view.ReadGeneration, view.Graph.ReadGeneration = nil, nil
	if !reflect.DeepEqual(view, want) {
		test.Fatal("serving substituted current candidate metadata")
	}
	notice, err := service.RepositoryChangeState(systemTestContext(), repo)
	if err != nil || notice.GraphProjection == nil || notice.GraphProjection.Generation != generation.Generation ||
		notice.GraphProjection.State != "catching_up" || !notice.GraphProjection.Complete || notice.GraphProjection.Current {
		test.Fatal("retained notice disagrees", notice, err)
	}
	if storage.candidateReads != 0 || storage.snapshotLists != 0 || storage.evidenceReads != 0 || len(storage.repairs) != 0 {
		test.Fatal("serving fell back to candidate/live data")
	}
}

func TestGraphServingRejectsCrossGenerationProofWithoutRepair(test *testing.T) {
	for _, kind := range []string{"repo", "dataset", "epoch", "sequence", "rules", "comparison"} {
		test.Run(kind, func(test *testing.T) {
			service, storage, repo, _ := newGraphProjectionRead(test)
			proof := domain.GraphQualificationResult{GraphQualificationKey: storage.result.QualificationKey(),
				Expected: storage.qualificationDigest, Actual: storage.qualificationDigest}
			switch kind {
			case "repo":
				proof.RepoID = hh("another repository")
				proof.Applied.RepoID = proof.RepoID
			case "dataset":
				proof.Dataset = "33333333-3333-4333-8333-333333333333"
			case "epoch":
				proof.Applied.Epoch = "33333333-3333-4333-8333-333333333333"
			case "sequence":
				proof.Applied.Sequence++
			case "rules":
				proof.Rules++
			case "comparison":
				proof.ComparisonRules++
			}
			storage.result.Qualification = &proof
			if _, err := service.GetRepositoryView(systemTestContext(), repo); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("foreign proof served", err)
			}
			if _, err := service.RepositoryChangeState(systemTestContext(), repo); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("foreign proof announced", err)
			}
			if len(storage.repairs) != 0 {
				test.Fatal("invalid proof triggered body repair")
			}
		})
	}
}

func TestGraphServingCorruptionRoutesOnlyToServingRepair(test *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(test)
	storage.result.Parts = storage.result.Parts[:1]
	_, err := service.GetRepositoryView(systemTestContext(), repo)
	var failure *domain.GraphProjectionCorruption
	if !errors.As(err, &failure) || !failure.Serving || storage.servingRepairs != 1 || len(storage.repairs) != 1 || !storage.repairs[0].Serving {
		test.Fatal("serving failure routed to candidate repair", err, storage.repairs)
	}
}
