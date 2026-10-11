package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func gitAdmissionSourceObservation(admission GitContributionAdmission) GitRefObservation {
	destination := admission.Destination
	return (GitRefObservation{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Source: "reconciliation",
		Ref: "refs/heads/" + admission.SourceBranch, After: admission.SourceHead,
		Provider: &GitProviderObservation{InstallationID: destination.InstallationID, RepositoryID: destination.ProviderRepositoryID}}).WithID()
}

func gitAdmissionRead(t *testing.T, destination GitDestinationCandidate, version int64, page int, head string, extra ...GitRefObservation) (GitDestinationCandidate, GitHeadReceipt) {
	t.Helper()
	observation := (GitRefObservation{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Source: "reconciliation",
		Ref: "refs/heads/" + destination.DestinationBranch, After: head,
		Provider: &GitProviderObservation{InstallationID: destination.InstallationID, RepositoryID: destination.ProviderRepositoryID}}).WithID()
	claim := GitHeadScan{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Version: version, Page: page, State: "running", LeaseUntil: time.Unix(500, 0).UTC()}
	receipt, err := NewGitHeadReceipt(claim, append([]GitRefObservation{observation}, extra...), false)
	if err != nil || receipt == nil || receipt.Validate() != nil {
		t.Fatalf("invalid read fixture: %+v %v", receipt, err)
	}
	destination.ObservationID, destination.DestinationHead = observation.ID, head
	destination.ReadReceiptID, destination.ReadVersion = receipt.ID, receipt.Version
	destination = destination.WithID()
	if destination.Validate() != nil || !destination.MatchesReceipt(*receipt) {
		t.Fatal("invalid destination fixture", destination)
	}
	return destination, *receipt
}

func gitAdmissionFixture(t *testing.T) (GitContributionAdmission, GitHeadReceipt) {
	t.Helper()
	admission := GitContributionAdmission{
		Destination: GitDestinationCandidate{RepoID: HashContent([]byte("admission repo")), GitOrigin: "https://github.com/acme/api.git",
			InstallationID: 17, ProviderRepositoryID: 29, DestinationBranchID: "main-incarnation", DestinationBranch: "main"},
		SourceBranchID: "feature-incarnation", SourceBranch: "feature", SourceAnchorID: strings.Repeat("1", 32), DestinationAnchorID: strings.Repeat("2", 32),
		PublicationID: strings.Repeat("3", 32), Capture: HashContent([]byte("original capture")), OriginalHead: strings.Repeat("a", 40), SourceHead: strings.Repeat("c", 40),
		OrdinaryObservationIDs: []string{strings.Repeat("4", 32)}, CarryObservationIDs: []string{strings.Repeat("5", 32)},
		GraphRevision: 7, EvidenceRevision: 11, AfterReadVersion: 6,
	}
	source := gitAdmissionSourceObservation(admission)
	destination, receipt := gitAdmissionRead(t, admission.Destination, 4, 1, strings.Repeat("b", 40), source)
	admission.Destination, admission.SourceObservationID = destination, source.ID
	admission = admission.WithID()
	if admission.Validate() != nil || !admission.MatchesBaseline(receipt) {
		t.Fatal("invalid admission fixture", admission)
	}
	return admission, receipt
}

func TestGitContributionAdmissionValidation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionAdmission)
	}{
		{"missing source identity", func(admission *GitContributionAdmission) { admission.SourceBranchID = "" }},
		{"same incarnation", func(admission *GitContributionAdmission) {
			admission.SourceBranchID = admission.Destination.DestinationBranchID
		}},
		{"same branch", func(admission *GitContributionAdmission) {
			admission.SourceBranch = admission.Destination.DestinationBranch
		}},
		{"invalid source branch", func(admission *GitContributionAdmission) { admission.SourceBranch = "bad..branch" }},
		{"invalid capture", func(admission *GitContributionAdmission) { admission.Capture = "capture" }},
		{"invalid original head", func(admission *GitContributionAdmission) { admission.OriginalHead = "HEAD" }},
		{"invalid source head", func(admission *GitContributionAdmission) { admission.SourceHead = "HEAD" }},
		{"mixed oid widths", func(admission *GitContributionAdmission) { admission.OriginalHead = strings.Repeat("a", 64) }},
		{"source equals baseline", func(admission *GitContributionAdmission) {
			admission.SourceHead = admission.Destination.DestinationHead
		}},
		{"watermark predates baseline", func(admission *GitContributionAdmission) {
			admission.AfterReadVersion = admission.Destination.ReadVersion - 1
		}},
		{"missing publication", func(admission *GitContributionAdmission) { admission.PublicationID = "" }},
		{"noncanonical publication", func(admission *GitContributionAdmission) { admission.PublicationID = strings.Repeat("A", 32) }},
		{"unqualified provider", func(admission *GitContributionAdmission) { admission.Destination.InstallationID = 0 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admission, receipt := gitAdmissionFixture(t)
			scenario.mutate(&admission)
			admission.SourceObservationID = gitAdmissionSourceObservation(admission).ID
			admission.Destination = admission.Destination.WithID()
			admission = admission.WithID()
			if !errors.Is(admission.Validate(), ErrValidation) || admission.MatchesBaseline(receipt) {
				t.Fatal("invalid admission accepted", admission)
			}
		})
	}
	admission, receipt := gitAdmissionFixture(t)
	admission.AfterReadVersion = admission.Destination.ReadVersion
	admission.OriginalHead = admission.Destination.DestinationHead
	admission = admission.WithID()
	if admission.Validate() != nil || !admission.MatchesBaseline(receipt) {
		t.Fatal("occurrence qualification incorrectly required a new original capture head")
	}
}

func TestGitContributionAdmissionRequiresCanonicalOrdinaryAndCarryIDs(t *testing.T) {
	for _, field := range []string{"ordinary", "carry"} {
		for _, scenario := range []string{"empty", "duplicate", "unsorted", "publication self", "invalid", "too many"} {
			t.Run(field+"/"+scenario, func(t *testing.T) {
				admission, _ := gitAdmissionFixture(t)
				ids := []string{}
				switch scenario {
				case "duplicate":
					ids = []string{strings.Repeat("4", 32), strings.Repeat("4", 32)}
				case "unsorted":
					ids = []string{strings.Repeat("5", 32), strings.Repeat("4", 32)}
				case "publication self":
					ids = []string{admission.PublicationID}
				case "invalid":
					ids = []string{strings.Repeat("g", 32)}
				case "too many":
					for number := 1; number <= 129; number++ {
						ids = append(ids, fmt.Sprintf("%032x", number))
					}
				}
				if field == "ordinary" {
					admission.OrdinaryObservationIDs = ids
				} else {
					admission.CarryObservationIDs = ids
				}
				if err := admission.WithID().Validate(); !errors.Is(err, ErrValidation) {
					t.Fatal("invalid proof IDs accepted", ids, err)
				}
			})
		}
	}
}

func TestGitContributionAdmissionBaselineRequiresSameQualifiedPage(t *testing.T) {
	admission, together := gitAdmissionFixture(t)
	if !admission.MatchesBaseline(together) {
		t.Fatal("same-page membership rejected")
	}
	destination, destinationPage := gitAdmissionRead(t, admission.Destination, 4, 1, admission.Destination.DestinationHead)
	admission.Destination = destination
	admission = admission.WithID()
	source := gitAdmissionSourceObservation(admission)
	sourcePage, err := NewGitHeadReceipt(GitHeadScan{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Version: 5, Page: 2, State: "running", LeaseUntil: time.Unix(500, 0).UTC()}, []GitRefObservation{source}, false)
	if err != nil || sourcePage == nil || sourcePage.Validate() != nil || admission.Validate() != nil || !destination.MatchesReceipt(destinationPage) {
		t.Fatal("invalid split-page fixture", err)
	}
	if admission.MatchesBaseline(destinationPage) || admission.MatchesBaseline(*sourcePage) {
		t.Fatal("source on another page qualified the destination baseline")
	}
	for _, mismatch := range []string{"source ref", "source head"} {
		t.Run(mismatch, func(t *testing.T) {
			admission, _ := gitAdmissionFixture(t)
			source := gitAdmissionSourceObservation(admission)
			if mismatch == "source ref" {
				source.Ref = "refs/heads/another-feature"
			} else {
				source.After = strings.Repeat("d", 40)
			}
			destination, page := gitAdmissionRead(t, admission.Destination, 4, 1, admission.Destination.DestinationHead, source.WithID())
			admission.Destination = destination
			admission = admission.WithID()
			if admission.Validate() != nil || admission.MatchesBaseline(page) {
				t.Fatal("inexact source membership accepted")
			}
		})
	}
	admission, legacy := gitAdmissionFixture(t)
	legacy.Provider = GitProviderObservation{}
	legacy = legacy.WithID()
	if admission.MatchesBaseline(legacy) || admission.MatchesBaseline(GitHeadReceipt{}) {
		t.Fatal("unqualified baseline accepted")
	}
}

func TestGitContributionAdmissionLaterReadStrictlyBeyondWatermark(t *testing.T) {
	admission, _ := gitAdmissionFixture(t)
	for _, version := range []int64{3, 4, 5, 6, 7} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			candidate, receipt := gitAdmissionRead(t, admission.Destination, version, 1, strings.Repeat("d", 40))
			if got, want := admission.MatchesLaterRead(candidate, receipt), version > admission.AfterReadVersion; got != want {
				t.Fatalf("later read=%v want=%v baseline=%d watermark=%d claim=%d", got, want, admission.Destination.ReadVersion, admission.AfterReadVersion, version)
			}
		})
	}
}

func TestGitContributionAdmissionSameHeadReturnRetainsOccurrenceIdentity(t *testing.T) {
	admission, baseline := gitAdmissionFixture(t)
	previous := admission.Destination
	for index, head := range []string{strings.Repeat("d", 40), admission.Destination.DestinationHead, admission.Destination.DestinationHead} {
		candidate, receipt := gitAdmissionRead(t, admission.Destination, admission.AfterReadVersion+int64(index)+1, 1, head)
		if !admission.MatchesLaterRead(candidate, receipt) || admission.MatchesBaseline(receipt) || receipt.ID == baseline.ID || receipt.ID == previous.ReadReceiptID || candidate.ID == previous.ID {
			t.Fatal("fresh occurrence conflated with earlier read", index, candidate)
		}
		if index > 0 && candidate.ObservationID != admission.Destination.ObservationID {
			t.Fatal("return to the same head changed immutable observation identity")
		}
		previous = candidate
	}
}

func TestGitContributionAdmissionLaterReadRejectsForeignScope(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitDestinationCandidate)
	}{
		{"repository", func(destination *GitDestinationCandidate) { destination.RepoID = HashContent([]byte("foreign repo")) }},
		{"origin", func(destination *GitDestinationCandidate) {
			destination.GitOrigin = "https://github.com/acme/other.git"
		}},
		{"installation", func(destination *GitDestinationCandidate) { destination.InstallationID++ }},
		{"provider repository", func(destination *GitDestinationCandidate) { destination.ProviderRepositoryID++ }},
		{"destination incarnation", func(destination *GitDestinationCandidate) { destination.DestinationBranchID = "reused-main" }},
		{"destination ref", func(destination *GitDestinationCandidate) { destination.DestinationBranch = "renamed-main" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admission, _ := gitAdmissionFixture(t)
			scope := admission.Destination
			scenario.mutate(&scope)
			candidate, receipt := gitAdmissionRead(t, scope, admission.AfterReadVersion+1, 1, strings.Repeat("d", 40))
			if admission.MatchesLaterRead(candidate, receipt) || admission.MatchesBaseline(receipt) {
				t.Fatal("foreign qualified read accepted", candidate)
			}
		})
	}
}

func TestGitContributionAdmissionRejectsTamperedCanonicalIDs(t *testing.T) {
	for _, scenario := range []string{"admission ID", "admission payload", "destination ID", "source observation ID", "receipt ID", "receipt payload", "receipt observation ID"} {
		t.Run(scenario, func(t *testing.T) {
			admission, receipt := gitAdmissionFixture(t)
			switch scenario {
			case "admission ID":
				admission.ID = strings.Repeat("f", 64)
			case "admission payload":
				admission.GraphRevision++
			case "destination ID":
				admission.Destination.ID = strings.Repeat("f", 64)
				admission = admission.WithID()
			case "source observation ID":
				admission.SourceObservationID = strings.Repeat("f", 64)
				admission = admission.WithID()
			case "receipt ID":
				receipt.ID = strings.Repeat("f", 64)
			case "receipt payload":
				receipt.More = !receipt.More
			case "receipt observation ID":
				receipt.Heads[0].ObservationID = strings.Repeat("f", 64)
				receipt = receipt.WithID()
			}
			if admission.Validate() == nil && receipt.Validate() == nil {
				t.Fatal("tampering retained valid canonical identities")
			}
			if admission.MatchesBaseline(receipt) {
				t.Fatal("tampered baseline qualified")
			}
		})
	}
	for _, scenario := range []string{"candidate ID", "receipt reference", "receipt ID", "unqualified provider"} {
		t.Run("later/"+scenario, func(t *testing.T) {
			admission, _ := gitAdmissionFixture(t)
			candidate, receipt := gitAdmissionRead(t, admission.Destination, admission.AfterReadVersion+1, 1, strings.Repeat("d", 40))
			switch scenario {
			case "candidate ID":
				candidate.ID = strings.Repeat("f", 64)
			case "receipt reference":
				candidate.ReadReceiptID = strings.Repeat("f", 64)
				candidate = candidate.WithID()
			case "receipt ID":
				receipt.ID = strings.Repeat("f", 64)
			case "unqualified provider":
				receipt.Provider = GitProviderObservation{}
				receipt = receipt.WithID()
			}
			if admission.MatchesLaterRead(candidate, receipt) {
				t.Fatal("tampered later read qualified")
			}
		})
	}
}

func TestGitContributionAdmissionLegacyAnchorRules(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		valid  bool
		mutate func(*GitContributionAdmission)
	}{
		{"modern source needs anchor", false, func(admission *GitContributionAdmission) { admission.SourceAnchorID = "" }},
		{"modern destination needs anchor", false, func(admission *GitContributionAdmission) { admission.DestinationAnchorID = "" }},
		{"legacy source", true, func(admission *GitContributionAdmission) {
			admission.SourceAnchorID = ""
			admission.SourceBranchID = LegacyContextBranchID(string(admission.Destination.RepoID), admission.SourceBranch)
		}},
		{"legacy destination", true, func(admission *GitContributionAdmission) {
			admission.DestinationAnchorID = ""
			admission.Destination.DestinationBranchID = LegacyContextBranchID(string(admission.Destination.RepoID), admission.Destination.DestinationBranch)
		}},
		{"both legacy", true, func(admission *GitContributionAdmission) {
			admission.SourceAnchorID, admission.DestinationAnchorID = "", ""
			admission.SourceBranchID = LegacyContextBranchID(string(admission.Destination.RepoID), admission.SourceBranch)
			admission.Destination.DestinationBranchID = LegacyContextBranchID(string(admission.Destination.RepoID), admission.Destination.DestinationBranch)
		}},
		{"legacy source wrong name", false, func(admission *GitContributionAdmission) {
			admission.SourceAnchorID = ""
			admission.SourceBranchID = LegacyContextBranchID(string(admission.Destination.RepoID), "old-feature")
		}},
		{"legacy destination wrong repo", false, func(admission *GitContributionAdmission) {
			admission.DestinationAnchorID = ""
			admission.Destination.DestinationBranchID = LegacyContextBranchID(string(HashContent([]byte("foreign repo"))), admission.Destination.DestinationBranch)
		}},
		{"malformed source anchor", false, func(admission *GitContributionAdmission) { admission.SourceAnchorID = strings.Repeat("F", 32) }},
		{"malformed destination anchor", false, func(admission *GitContributionAdmission) { admission.DestinationAnchorID = strings.Repeat("g", 32) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admission, receipt := gitAdmissionFixture(t)
			scenario.mutate(&admission)
			admission.Destination = admission.Destination.WithID()
			admission = admission.WithID()
			if got := admission.Validate() == nil; got != scenario.valid || admission.MatchesBaseline(receipt) != scenario.valid {
				t.Fatalf("anchor qualification=%v want=%v: %+v", got, scenario.valid, admission)
			}
		})
	}
}

func TestGitContributionAdmissionKeyStableAcrossReadMetadata(t *testing.T) {
	for _, scenario := range []string{"receipt version", "receipt page", "graph revision", "evidence revision", "watermark"} {
		t.Run(scenario, func(t *testing.T) {
			admission, receipt := gitAdmissionFixture(t)
			original := admission
			switch scenario {
			case "receipt version":
				admission.Destination, receipt = gitAdmissionRead(t, admission.Destination, admission.Destination.ReadVersion+1, 1, admission.Destination.DestinationHead, gitAdmissionSourceObservation(admission))
			case "receipt page":
				admission.Destination, receipt = gitAdmissionRead(t, admission.Destination, admission.Destination.ReadVersion+1, 2, admission.Destination.DestinationHead, gitAdmissionSourceObservation(admission))
			case "graph revision":
				admission.GraphRevision++
			case "evidence revision":
				admission.EvidenceRevision++
			case "watermark":
				admission.AfterReadVersion++
			}
			admission = admission.WithID()
			if admission.Validate() != nil || !admission.MatchesBaseline(receipt) || admission.Key() != original.Key() || admission.ID == original.ID || admission.ID != admission.WithID().ID {
				t.Fatal("read metadata changed admission key or lost immutable payload identity", admission, original)
			}
		})
	}
}

func TestGitContributionAdmissionSemanticChangesHaveDistinctKeys(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionAdmission)
	}{
		{"source incarnation", func(admission *GitContributionAdmission) { admission.SourceBranchID = "new-feature" }},
		{"destination incarnation", func(admission *GitContributionAdmission) { admission.Destination.DestinationBranchID = "new-main" }},
		{"publication", func(admission *GitContributionAdmission) { admission.PublicationID = strings.Repeat("6", 32) }},
		{"capture", func(admission *GitContributionAdmission) { admission.Capture = HashContent([]byte("another capture")) }},
		{"original head", func(admission *GitContributionAdmission) { admission.OriginalHead = strings.Repeat("e", 40) }},
		{"carrying head", func(admission *GitContributionAdmission) { admission.SourceHead = strings.Repeat("d", 40) }},
		{"repository", func(admission *GitContributionAdmission) {
			admission.Destination.RepoID = HashContent([]byte("another repo"))
		}},
		{"origin", func(admission *GitContributionAdmission) {
			admission.Destination.GitOrigin = "https://github.com/acme/other.git"
		}},
		{"installation", func(admission *GitContributionAdmission) { admission.Destination.InstallationID++ }},
		{"provider repository", func(admission *GitContributionAdmission) { admission.Destination.ProviderRepositoryID++ }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admission, _ := gitAdmissionFixture(t)
			original := admission
			scenario.mutate(&admission)
			source := gitAdmissionSourceObservation(admission)
			destination, receipt := gitAdmissionRead(t, admission.Destination, admission.Destination.ReadVersion, 1, admission.Destination.DestinationHead, source)
			admission.Destination, admission.SourceObservationID = destination, source.ID
			admission = admission.WithID()
			if admission.Validate() != nil || !admission.MatchesBaseline(receipt) || admission.Key() == original.Key() || admission.ID == original.ID {
				t.Fatal("semantic change shared admission identity", admission, original)
			}
		})
	}
}
