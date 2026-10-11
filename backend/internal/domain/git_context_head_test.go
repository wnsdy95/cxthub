package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func gitContextHeadFixture(t *testing.T) (GitContributionAdmission, GitContextHead, GitHeadReceipt, GitContributionProof) {
	t.Helper()
	admission, _ := gitAdmissionFixture(t)
	destination, receipt := gitAdmissionRead(t, admission.Destination, admission.AfterReadVersion+1, 2, strings.Repeat("e", 40))
	head := GitContextHead{Destination: destination, AnchorID: admission.DestinationAnchorID,
		ContextTarget: HashContent([]byte("destination capture at read")), GraphRevision: 15, EvidenceRevision: 21}.WithID()
	contribution := GitContribution{RepoID: admission.Destination.RepoID, GitOrigin: admission.Destination.GitOrigin,
		ProviderRepositoryID: admission.Destination.ProviderRepositoryID, DestinationBranchID: admission.Destination.DestinationBranchID,
		SourceBranchID: admission.SourceBranchID, Capture: admission.Capture, OriginalHead: admission.OriginalHead,
		IntegrationSHA: strings.Repeat("d", 40)}.WithID()
	proof := GitContributionProof{AdmissionID: admission.ID, ContextHeadID: head.ID, Contribution: contribution}.WithID()
	if head.Validate() != nil || proof.Validate() != nil || !proof.MatchesInputs(admission, head, receipt) {
		t.Fatal("invalid context-head fixture")
	}
	return admission, head, receipt, proof
}

func TestGitContextHeadIdentityBindsFrozenRead(t *testing.T) {
	_, head, _, _ := gitContextHeadFixture(t)
	if len(head.ID) != 64 || head.WithID() != head {
		t.Fatal("unstable context-head identity")
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContextHead)
	}{
		{"anchor", func(next *GitContextHead) { next.AnchorID = strings.Repeat("f", 32) }},
		{"context target", func(next *GitContextHead) { next.ContextTarget = HashContent([]byte("reset target")) }},
		{"graph revision", func(next *GitContextHead) { next.GraphRevision++ }},
		{"evidence revision", func(next *GitContextHead) { next.EvidenceRevision++ }},
		{"read occurrence", func(next *GitContextHead) {
			next.Destination.ReadVersion++
			next.Destination = next.Destination.WithID()
		}},
		{"receipt", func(next *GitContextHead) {
			next.Destination.ReadReceiptID = strings.Repeat("f", 64)
			next.Destination = next.Destination.WithID()
		}},
		{"context incarnation", func(next *GitContextHead) {
			next.Destination.DestinationBranchID = "reused-main-incarnation"
			next.Destination = next.Destination.WithID()
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			next := head
			scenario.mutate(&next)
			if !errors.Is(next.Validate(), ErrValidation) {
				t.Fatal("mutated read retained a valid canonical ID")
			}
			next = next.WithID()
			if next.ID == head.ID || next.Validate() != nil {
				t.Fatal("frozen field excluded from identity", next)
			}
		})
	}
	tampered := head
	tampered.ID = "untrusted prior ID"
	if tampered.WithID() != head {
		t.Fatal("head ID participates in its own hash")
	}
}

func TestGitContextHeadValidation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContextHead)
	}{
		{"missing target", func(head *GitContextHead) { head.ContextTarget = "" }},
		{"malformed target", func(head *GitContextHead) { head.ContextTarget = "sha256:bad" }},
		{"noncanonical target", func(head *GitContextHead) { head.ContextTarget = ContentHash("sha256:" + strings.Repeat("A", 64)) }},
		{"missing explicit anchor", func(head *GitContextHead) { head.AnchorID = "" }},
		{"short anchor", func(head *GitContextHead) { head.AnchorID = strings.Repeat("a", 31) }},
		{"long anchor", func(head *GitContextHead) { head.AnchorID = strings.Repeat("a", 33) }},
		{"uppercase anchor", func(head *GitContextHead) { head.AnchorID = strings.Repeat("A", 32) }},
		{"nonhex anchor", func(head *GitContextHead) { head.AnchorID = strings.Repeat("g", 32) }},
		{"tampered candidate", func(head *GitContextHead) { head.Destination.ReadVersion++ }},
		{"unqualified candidate", func(head *GitContextHead) {
			head.Destination.InstallationID = 0
			head.Destination = head.Destination.WithID()
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, head, _, _ := gitContextHeadFixture(t)
			scenario.mutate(&head)
			if err := head.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatal("invalid context head accepted", head, err)
			}
		})
	}
	for _, id := range []string{"", strings.Repeat("f", 64), strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 64)} {
		_, head, _, _ := gitContextHeadFixture(t)
		head.ID = id
		if !errors.Is(head.Validate(), ErrValidation) {
			t.Fatal("invalid canonical head ID accepted", id)
		}
	}
}

func TestGitContextHeadLegacyAnchorRequiresExactScope(t *testing.T) {
	_, head, _, _ := gitContextHeadFixture(t)
	head.Destination.DestinationBranchID = LegacyContextBranchID(string(head.Destination.RepoID), head.Destination.DestinationBranch)
	head.Destination = head.Destination.WithID()
	head.AnchorID = ""
	head = head.WithID()
	if head.Validate() != nil {
		t.Fatal("legacy read required a fabricated anchor")
	}
	for _, scenario := range []string{"repository", "branch name", "explicit incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			next := head
			switch scenario {
			case "repository":
				next.Destination.RepoID = HashContent([]byte("other repo"))
			case "branch name":
				next.Destination.DestinationBranch = "other-main"
			case "explicit incarnation":
				next.Destination.DestinationBranchID = "main-incarnation"
			}
			next.Destination = next.Destination.WithID()
			if !errors.Is(next.WithID().Validate(), ErrValidation) {
				t.Fatal("anchorless legacy binding escaped its exact scope")
			}
		})
	}
}

func TestGitContributionProofValidationAndIdentity(t *testing.T) {
	_, _, _, proof := gitContextHeadFixture(t)
	if len(proof.ID) != 64 || proof.WithID() != proof {
		t.Fatal("unstable proof identity")
	}
	for _, field := range []string{"admission", "context head"} {
		for _, id := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), "sha256:" + strings.Repeat("a", 64)} {
			next := proof
			if field == "admission" {
				next.AdmissionID = id
			} else {
				next.ContextHeadID = id
			}
			if !errors.Is(next.WithID().Validate(), ErrValidation) {
				t.Fatal("invalid proof reference accepted", field, id)
			}
		}
	}
	for _, field := range []string{"admission", "context head", "contribution"} {
		next := proof
		switch field {
		case "admission":
			next.AdmissionID = strings.Repeat("f", 64)
		case "context head":
			next.ContextHeadID = strings.Repeat("f", 64)
		case "contribution":
			next.Contribution.IntegrationSHA = strings.Repeat("f", 40)
			next.Contribution = next.Contribution.WithID()
		}
		if !errors.Is(next.Validate(), ErrValidation) || next.WithID().ID == proof.ID || next.WithID().Validate() != nil {
			t.Fatal("proof identity omitted a payload or reference", field)
		}
	}
	invalid := proof
	invalid.Contribution.Capture = HashContent([]byte("tampered nested contribution"))
	if !errors.Is(invalid.WithID().Validate(), ErrValidation) {
		t.Fatal("valid outer ID concealed invalid nested contribution")
	}
	invalid = proof
	invalid.ID = "untrusted prior ID"
	if invalid.Validate() == nil || invalid.WithID() != proof {
		t.Fatal("proof canonical ID handling changed")
	}
}

func TestGitContributionProofMatchesExactContributionInputs(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContribution)
	}{
		{"repository", func(record *GitContribution) { record.RepoID = HashContent([]byte("other repo")) }},
		{"origin", func(record *GitContribution) { record.GitOrigin += "-other" }},
		{"provider repository", func(record *GitContribution) { record.ProviderRepositoryID++ }},
		{"destination incarnation", func(record *GitContribution) { record.DestinationBranchID = "other-main" }},
		{"source incarnation", func(record *GitContribution) { record.SourceBranchID = "other-feature" }},
		{"capture", func(record *GitContribution) { record.Capture = HashContent([]byte("other capture")) }},
		{"original head", func(record *GitContribution) { record.OriginalHead = strings.Repeat("f", 40) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admission, head, receipt, proof := gitContextHeadFixture(t)
			scenario.mutate(&proof.Contribution)
			proof.Contribution = proof.Contribution.WithID()
			proof = proof.WithID()
			if proof.Validate() != nil {
				t.Fatal("fixture mutation should remain structurally valid")
			}
			if proof.MatchesInputs(admission, head, receipt) {
				t.Fatal("foreign contribution matched frozen inputs", proof)
			}
		})
	}
}

func TestGitContributionProofRejectsTamperedOrMismatchedReferences(t *testing.T) {
	for _, scenario := range []string{"proof ID", "admission payload", "head payload", "receipt payload", "admission reference", "head reference", "different page", "missing destination"} {
		t.Run(scenario, func(t *testing.T) {
			admission, head, receipt, proof := gitContextHeadFixture(t)
			switch scenario {
			case "proof ID":
				proof.ID = strings.Repeat("f", 64)
			case "admission payload":
				admission.AfterReadVersion++
			case "head payload":
				head.ContextTarget = HashContent([]byte("later reset"))
			case "receipt payload":
				receipt.More = !receipt.More
			case "admission reference":
				proof.AdmissionID = strings.Repeat("f", 64)
				proof = proof.WithID()
			case "head reference":
				proof.ContextHeadID = strings.Repeat("f", 64)
				proof = proof.WithID()
			case "different page":
				receipt.Page++
				receipt = receipt.WithID()
			case "missing destination":
				scope := head.Destination
				scope.DestinationBranch = "other-main"
				_, receipt = gitAdmissionRead(t, scope, receipt.Version, receipt.Page, scope.DestinationHead)
			}
			if proof.MatchesInputs(admission, head, receipt) {
				t.Fatal("tampered or different immutable input matched", scenario)
			}
		})
	}
}

func TestGitContributionProofRequiresQualifiedSameDestinationIncarnation(t *testing.T) {
	for _, scenario := range []string{"repository", "origin", "installation", "provider repository", "incarnation", "renamed reused incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			admission, head, _, proof := gitContextHeadFixture(t)
			scope := head.Destination
			switch scenario {
			case "repository":
				scope.RepoID = HashContent([]byte("other repo"))
			case "origin":
				scope.GitOrigin += "-other"
			case "installation":
				scope.InstallationID++
			case "provider repository":
				scope.ProviderRepositoryID++
			case "incarnation":
				scope.DestinationBranchID = "reused-main-incarnation"
			case "renamed reused incarnation":
				scope.DestinationBranch = "other-main"
				scope.DestinationBranchID = "reused-main-incarnation"
			}
			destination, receipt := gitAdmissionRead(t, scope, scope.ReadVersion, 2, scope.DestinationHead)
			head.Destination = destination
			head = head.WithID()
			proof.ContextHeadID = head.ID
			proof = proof.WithID()
			if head.Validate() != nil || proof.Validate() != nil || !head.Destination.MatchesReceipt(receipt) {
				t.Fatal("foreign-read fixture should be independently valid")
			}
			if admission.MatchesLaterRead(head.Destination, receipt) || admission.MatchesLaterContextHead(head, receipt) || proof.MatchesInputs(admission, head, receipt) {
				t.Fatal("foreign provider/read incarnation matched admission")
			}
		})
	}
}

func TestGitContextHeadBoundReadAllowsRenameWithoutWeakeningStrictRead(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		admission, head, _, proof := gitContextHeadFixture(t)
		scope := head.Destination
		if renamed {
			scope.DestinationBranch = "renamed-main"
			head.AnchorID = strings.Repeat("9", 32)
		}
		for _, version := range []int64{admission.AfterReadVersion, admission.AfterReadVersion + 1} {
			destination, receipt := gitAdmissionRead(t, scope, version, 2, scope.DestinationHead)
			head.Destination = destination
			head = head.WithID()
			proof.ContextHeadID = head.ID
			proof = proof.WithID()
			if head.Validate() != nil || proof.Validate() != nil || !head.Destination.MatchesReceipt(receipt) {
				t.Fatal("bound read fixture must be independently valid")
			}
			later := version > admission.AfterReadVersion
			if got := admission.MatchesLaterRead(destination, receipt); got != (later && !renamed) {
				t.Fatal("strict candidate matching accepted a rename or stale read", renamed, version, got)
			}
			if got := admission.MatchesLaterContextHead(head, receipt); got != later {
				t.Fatal("bound same-incarnation matching lost rename or watermark semantics", renamed, version, got)
			}
			if got := proof.MatchesInputs(admission, head, receipt); got != later {
				t.Fatal("proof did not use bound context-head matching", renamed, version, got)
			}
		}
	}
}

func TestGitContextHeadBoundRenameRequiresValidExactRead(t *testing.T) {
	for _, scenario := range []string{"head ID", "anchor", "context target", "old-name receipt", "different page"} {
		t.Run(scenario, func(t *testing.T) {
			admission, head, oldReceipt, proof := gitContextHeadFixture(t)
			scope := head.Destination
			scope.DestinationBranch = "renamed-main"
			destination, receipt := gitAdmissionRead(t, scope, scope.ReadVersion, 2, scope.DestinationHead)
			head.Destination, head.AnchorID = destination, strings.Repeat("9", 32)
			head = head.WithID()
			switch scenario {
			case "head ID":
				head.ID = strings.Repeat("f", 64)
			case "anchor":
				head.AnchorID = ""
				head = head.WithID()
			case "context target":
				head.ContextTarget = ""
				head = head.WithID()
			case "old-name receipt":
				receipt = oldReceipt
			case "different page":
				receipt.Page++
				receipt = receipt.WithID()
			}
			proof.ContextHeadID = head.ID
			proof = proof.WithID()
			if admission.MatchesLaterContextHead(head, receipt) || proof.MatchesInputs(admission, head, receipt) {
				t.Fatal("rename bypassed bound-head validation or exact receipt membership")
			}
		})
	}
}

func TestGitContributionProofRequiresReadStrictlyBeyondWatermark(t *testing.T) {
	admission, head, _, proof := gitContextHeadFixture(t)
	var observationID string
	seenReceipts := map[string]bool{}
	for _, version := range []int64{admission.Destination.ReadVersion, admission.AfterReadVersion, admission.AfterReadVersion + 1} {
		destination, receipt := gitAdmissionRead(t, admission.Destination, version, 2, head.Destination.DestinationHead)
		if observationID != "" && observationID != destination.ObservationID {
			t.Fatal("identical ref fact unexpectedly changed observation identity")
		}
		observationID = destination.ObservationID
		if seenReceipts[receipt.ID] {
			t.Fatal("different read occurrences shared a receipt")
		}
		seenReceipts[receipt.ID] = true
		head.Destination = destination
		head = head.WithID()
		proof.ContextHeadID = head.ID
		proof = proof.WithID()
		if got := proof.MatchesInputs(admission, head, receipt); got != (version > admission.AfterReadVersion) {
			t.Fatal("observation identity bypassed read watermark", version, got)
		}
	}
}

func TestGitContextHeadAndProofRoundTripPreserveHistoricalBinding(t *testing.T) {
	admission, head, receipt, proof := gitContextHeadFixture(t)
	head.GraphRevision, head.EvidenceRevision = math.MaxUint64, math.MaxUint64
	head = head.WithID()
	proof.ContextHeadID = head.ID
	proof = proof.WithID()
	if len(receipt.Heads) != 1 || receipt.Heads[0].Ref != "refs/heads/"+admission.Destination.DestinationBranch || proof.Contribution.IntegrationSHA == head.Destination.DestinationHead {
		t.Fatal("fixture requires a destination-only read after the introduction commit")
	}
	for _, value := range []any{head, proof} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		switch value.(type) {
		case GitContextHead:
			var decoded GitContextHead
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded != head || decoded.Validate() != nil {
				t.Fatal("historical head changed during round trip", decoded, err)
			}
			head = decoded
		case GitContributionProof:
			var decoded GitContributionProof
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded != proof || decoded.Validate() != nil {
				t.Fatal("proof changed during round trip", decoded, err)
			}
			proof = decoded
		}
	}
	if !proof.MatchesInputs(admission, head, receipt) {
		t.Fatal("bound destination read required current source membership or equal revision/tip")
	}
}
