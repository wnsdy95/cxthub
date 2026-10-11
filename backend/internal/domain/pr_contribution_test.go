package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func prContributionFixture() PRContribution {
	return PRContribution{
		RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/example/project",
		ProviderRepositoryID: 123, PullRequestID: 456,
		PR:                  PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)},
		DestinationBranchID: "main-identity", SourceBranchID: "feature-identity",
		Capture: HashContent([]byte("capture")), OriginalHead: strings.Repeat("c", 40),
	}.WithID()
}

func TestPRContributionIdentity(t *testing.T) {
	contribution := prContributionFixture()
	if err := contribution.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(contribution.ID) != 64 || contribution.WithID() != contribution {
		t.Fatal("unstable content key", contribution)
	}
	if contribution.ID != "f094951636e9d671bfcdd5d60ac7b6e1da084e6bb9fd5dc35f753b0370e4d846" {
		t.Fatal("version-1 contribution encoding changed", contribution.ID)
	}
	mutations := map[string]func(*PRContribution){
		"repo":          func(next *PRContribution) { next.RepoID = HashContent([]byte("other repo")) },
		"origin":        func(next *PRContribution) { next.GitOrigin += "-other" },
		"provider":      func(next *PRContribution) { next.ProviderRepositoryID++ },
		"pull-request":  func(next *PRContribution) { next.PullRequestID++ },
		"number":        func(next *PRContribution) { next.PR.Number++ },
		"base":          func(next *PRContribution) { next.PR.BaseBranch = "release" },
		"head":          func(next *PRContribution) { next.PR.HeadBranch = "other-feature" },
		"head-sha":      func(next *PRContribution) { next.PR.HeadSHA = strings.Repeat("d", 40) },
		"merge-sha":     func(next *PRContribution) { next.PR.MergeSHA = strings.Repeat("d", 40) },
		"destination":   func(next *PRContribution) { next.DestinationBranchID = "other-main" },
		"source":        func(next *PRContribution) { next.SourceBranchID = "other-feature" },
		"capture":       func(next *PRContribution) { next.Capture = HashContent([]byte("other capture")) },
		"original-head": func(next *PRContribution) { next.OriginalHead = strings.Repeat("d", 40) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			next := contribution
			mutate(&next)
			if !errors.Is(next.Validate(), ErrValidation) {
				t.Fatal("altered payload retained its key")
			}
			next = next.WithID()
			if next.ID == contribution.ID || next.Validate() != nil {
				t.Fatal("semantic field not included in key", next)
			}
		})
	}
	contribution.ID = "untrusted prior key"
	if contribution.WithID() != prContributionFixture() {
		t.Fatal("ID included in its own key")
	}
	raw, err := json.Marshal(contribution.WithID())
	if err != nil {
		t.Fatal(err)
	}
	var decoded PRContribution
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != contribution.WithID() {
		t.Fatal("serialization changed identity", err)
	}
}

func TestPRContributionValidation(t *testing.T) {
	mutations := map[string]func(*PRContribution){
		"repo":              func(next *PRContribution) { next.RepoID = "sha256:../repo" },
		"capture":           func(next *PRContribution) { next.Capture = ContentHash("sha256:" + strings.Repeat("A", 64)) },
		"origin-empty":      func(next *PRContribution) { next.GitOrigin = "" },
		"origin-whitespace": func(next *PRContribution) { next.GitOrigin = " \t" },
		"origin-control":    func(next *PRContribution) { next.GitOrigin += "\x00" },
		"origin-utf8":       func(next *PRContribution) { next.GitOrigin = string([]byte{0xff}) },
		"provider":          func(next *PRContribution) { next.ProviderRepositoryID = 0 },
		"pull-request":      func(next *PRContribution) { next.PullRequestID = -1 },
		"number":            func(next *PRContribution) { next.PR.Number = 0 },
		"base":              func(next *PRContribution) { next.PR.BaseBranch = "../main" },
		"same-branches":     func(next *PRContribution) { next.PR.HeadBranch = next.PR.BaseBranch },
		"head-sha":          func(next *PRContribution) { next.PR.HeadSHA = strings.Repeat("A", 40) },
		"zero-head":         func(next *PRContribution) { next.PR.HeadSHA = strings.Repeat("0", 40) },
		"zero-merge":        func(next *PRContribution) { next.PR.MergeSHA = strings.Repeat("0", 40) },
		"merge-width":       func(next *PRContribution) { next.PR.MergeSHA = strings.Repeat("b", 64) },
		"destination":       func(next *PRContribution) { next.DestinationBranchID = "" },
		"destination-size":  func(next *PRContribution) { next.DestinationBranchID = strings.Repeat("a", 129) },
		"source":            func(next *PRContribution) { next.SourceBranchID = "\ninvalid" },
		"source-size":       func(next *PRContribution) { next.SourceBranchID = strings.Repeat("a", 129) },
		"original-head":     func(next *PRContribution) { next.OriginalHead = strings.Repeat("g", 40) },
		"original-width":    func(next *PRContribution) { next.OriginalHead = strings.Repeat("a", 64) },
		"original-zero":     func(next *PRContribution) { next.OriginalHead = strings.Repeat("0", 40) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			contribution := prContributionFixture()
			mutate(&contribution)
			if err := contribution.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatal("invalid contribution accepted", err)
			}
		})
	}
	for _, id := range []string{"", strings.Repeat("a", 64), strings.ToUpper(prContributionFixture().ID), "../outside"} {
		contribution := prContributionFixture()
		contribution.ID = id
		if !errors.Is(contribution.Validate(), ErrValidation) {
			t.Fatal("invalid key accepted", id)
		}
	}
	contribution := prContributionFixture()
	contribution.PR.HeadSHA = strings.Repeat("a", 64)
	contribution.PR.MergeSHA = strings.Repeat("b", 64)
	contribution.OriginalHead = strings.Repeat("c", 64)
	if err := contribution.WithID().Validate(); err != nil {
		t.Fatal("SHA-256 Git IDs rejected", err)
	}
}

func TestPRContributionMemoryIdentityAndValidation(t *testing.T) {
	contribution := prContributionFixture()
	memory := PRContributionMemory{RepoID: contribution.RepoID, ContributionID: contribution.ID, PublicationID: strings.Repeat("d", 32), MemoryHash: HashContent([]byte("memory-v1")), MemorySource: contribution.Capture, MemoryPinned: true}.WithID()
	if memory.Validate() != nil || len(memory.ID) != 64 || memory.WithID() != memory {
		t.Fatal("invalid memory fixture", memory)
	}
	if memory.ID != "d3c6a12eaa61b3ad33d74a475e456235e0ab56b671a2b62c3d0414622e2aa141" {
		t.Fatal("version-1 memory encoding changed", memory.ID)
	}
	mutations := map[string]func(*PRContributionMemory){
		"repo":         func(next *PRContributionMemory) { next.RepoID = HashContent([]byte("other repo")) },
		"contribution": func(next *PRContributionMemory) { next.ContributionID = strings.Repeat("e", 64) },
		"publication":  func(next *PRContributionMemory) { next.PublicationID = strings.Repeat("f", 32) },
		"memory":       func(next *PRContributionMemory) { next.MemoryHash = HashContent([]byte("memory-v2")) },
		"source":       func(next *PRContributionMemory) { next.MemorySource = HashContent([]byte("other source")) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			next := memory
			mutate(&next)
			if next.Validate() == nil || next.WithID().ID == memory.ID || next.WithID().Validate() != nil {
				t.Fatal("memory version not immutable", next)
			}
		})
	}
	invalid := []PRContributionMemory{memory, memory, memory, memory, memory, memory, memory, memory}
	invalid[0].RepoID = "bad"
	invalid[1].MemorySource = ""
	invalid[2].ContributionID = strings.Repeat("A", 64)
	invalid[3].ContributionID = strings.Repeat("a", 63)
	invalid[4].PublicationID = strings.Repeat("A", 32)
	invalid[5].PublicationID = strings.Repeat("g", 32)
	invalid[6].PublicationID = strings.Repeat("a", 31)
	invalid[7].MemoryHash = ContentHash("sha256:" + strings.Repeat("F", 64))
	for index, next := range invalid {
		if err := next.WithID().Validate(); !errors.Is(err, ErrValidation) {
			t.Fatal("invalid memory accepted", index, err)
		}
	}
	unpinned := memory
	unpinned.MemoryPinned = false
	if unpinned.WithID().ID == memory.ID || unpinned.WithID().Validate() == nil {
		t.Fatal("unpinned memory accepted or excluded from key")
	}
	empty := memory
	empty.MemoryHash, empty.MemorySource = "", ""
	if empty.WithID().Validate() != nil {
		t.Fatal("explicit empty memory rejected")
	}
	empty.MemorySource = memory.MemorySource
	if empty.WithID().Validate() != nil {
		t.Fatal("explicit empty memory with source rejected")
	}
	empty.MemoryPinned = false
	if empty.WithID().Validate() == nil {
		t.Fatal("unpinned empty memory accepted")
	}
	invalidSource := memory
	invalidSource.MemorySource = "bad"
	if invalidSource.WithID().Validate() == nil {
		t.Fatal("invalid source accepted")
	}
	memory.ID = ""
	if memory.Validate() == nil {
		t.Fatal("empty ID accepted")
	}
}
