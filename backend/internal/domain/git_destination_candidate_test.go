package domain

import (
	"strings"
	"testing"
)

func destinationWitnessFixture() GitDestinationCandidate {
	checkpoint, _, _, _ := participationFixture()
	return (GitDestinationCandidate{RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, InstallationID: 789,
		ProviderRepositoryID: 91, ObservationID: strings.Repeat("d", 64), DestinationBranchID: "main-identity",
		ReadReceiptID: strings.Repeat("b", 64), ReadVersion: 1, DestinationBranch: "main", DestinationHead: strings.Repeat("e", 40)}).WithID()
}

func TestGitDestinationCandidateRejectsIncompleteIdentity(t *testing.T) {
	for _, scenario := range []string{"installation", "provider", "observation", "uppercase-observation", "missing-receipt", "missing-read-version", "branch", "head", "tampered"} {
		t.Run(scenario, func(t *testing.T) {
			witness := destinationWitnessFixture()
			if witness.Validate() != nil {
				t.Fatal("invalid fixture")
			}
			switch scenario {
			case "installation":
				witness.InstallationID = 0
			case "provider":
				witness.ProviderRepositoryID = 0
			case "observation":
				witness.ObservationID = "../outside"
			case "uppercase-observation":
				witness.ObservationID = strings.ToUpper(witness.ObservationID)
			case "missing-receipt":
				witness.ReadReceiptID = ""
			case "missing-read-version":
				witness.ReadVersion = 0
			case "branch":
				witness.DestinationBranch = "refs/heads/../bad"
			case "head":
				witness.DestinationHead = "123abcd"
			case "tampered":
				witness.DestinationHead = strings.Repeat("f", 40)
			}
			if scenario != "tampered" {
				witness = witness.WithID()
			}
			if witness.Validate() == nil {
				t.Fatal("invalid witness accepted")
			}
		})
	}
}

func TestGitDestinationCandidateRequiresQualifiedMatchingObservation(t *testing.T) {
	witness := destinationWitnessFixture()
	observation := (GitRefObservation{RepoID: witness.RepoID, GitOrigin: witness.GitOrigin, Source: "reconciliation",
		Ref: "refs/heads/" + witness.DestinationBranch, After: witness.DestinationHead,
		Provider: &GitProviderObservation{InstallationID: witness.InstallationID, RepositoryID: witness.ProviderRepositoryID}}).WithID()
	witness.ObservationID = observation.ID
	witness = witness.WithID()
	if !witness.MatchesObservation(observation) {
		t.Fatal("qualified matching observation rejected")
	}
	for _, scenario := range []string{"legacy", "delayed-push", "installation", "repository", "destination", "head", "origin"} {
		t.Run(scenario, func(t *testing.T) {
			next := observation
			provider := *observation.Provider
			next.Provider = &provider
			switch scenario {
			case "legacy":
				next.Provider = nil
			case "delayed-push":
				next.Source, next.Delivery = "github-push", "historical-delivery"
			case "installation":
				next.Provider.InstallationID++
			case "repository":
				next.Provider.RepositoryID++
			case "destination":
				next.Ref = "refs/heads/other"
			case "head":
				next.After = strings.Repeat("c", 40)
			case "origin":
				next.GitOrigin += "-other"
			}
			next = next.WithID()
			candidate := witness
			candidate.ObservationID = next.ID
			candidate = candidate.WithID()
			if candidate.MatchesObservation(next) {
				t.Fatal("observation without matching provider and destination accepted")
			}
		})
	}
}
