package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func gitContributionFixture() GitContribution {
	return GitContribution{
		RepoID: HashContent([]byte("repo")), GitOrigin: "https://github.com/example/project",
		ProviderRepositoryID: 123, IntegrationSHA: strings.Repeat("b", 40),
		DestinationBranchID: "main-identity", SourceBranchID: "feature-identity",
		Capture: HashContent([]byte("capture")), OriginalHead: strings.Repeat("c", 40),
	}.WithID()
}

func TestGitContributionIdentity(t *testing.T) {
	contribution := gitContributionFixture()
	if err := contribution.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(contribution.ID) != 64 || contribution.WithID() != contribution {
		t.Fatal("unstable content key", contribution)
	}
	if contribution.ID != "64f8891fab0f1883d7f590b9fb173ce9c26e5cbf6185a1c9de7f0bb800568bd0" {
		t.Fatal("version-1 contribution encoding changed", contribution.ID)
	}
	mutations := map[string]func(*GitContribution){
		"integration":   func(next *GitContribution) { next.IntegrationSHA = strings.Repeat("d", 40) },
		"repo":          func(next *GitContribution) { next.RepoID = HashContent([]byte("other repo")) },
		"origin":        func(next *GitContribution) { next.GitOrigin += "-other" },
		"provider":      func(next *GitContribution) { next.ProviderRepositoryID++ },
		"destination":   func(next *GitContribution) { next.DestinationBranchID = "other-main" },
		"source":        func(next *GitContribution) { next.SourceBranchID = "other-feature" },
		"capture":       func(next *GitContribution) { next.Capture = HashContent([]byte("other capture")) },
		"original-head": func(next *GitContribution) { next.OriginalHead = strings.Repeat("d", 40) },
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
	if contribution.WithID() != gitContributionFixture() {
		t.Fatal("ID included in its own key")
	}
	raw, err := json.Marshal(contribution.WithID())
	if err != nil {
		t.Fatal(err)
	}
	var decoded GitContribution
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != contribution.WithID() {
		t.Fatal("serialization changed identity", err)
	}
}

func TestGitContributionValidation(t *testing.T) {
	mutations := map[string]func(*GitContribution){
		"repo":              func(next *GitContribution) { next.RepoID = "sha256:../repo" },
		"capture":           func(next *GitContribution) { next.Capture = ContentHash("sha256:" + strings.Repeat("A", 64)) },
		"origin-empty":      func(next *GitContribution) { next.GitOrigin = "" },
		"origin-whitespace": func(next *GitContribution) { next.GitOrigin = " \t" },
		"origin-control":    func(next *GitContribution) { next.GitOrigin += "\x00" },
		"origin-utf8":       func(next *GitContribution) { next.GitOrigin = string([]byte{0xff}) },
		"provider":          func(next *GitContribution) { next.ProviderRepositoryID = 0 },
		"negative-provider": func(next *GitContribution) { next.ProviderRepositoryID = -1 },
		"integration-zero":  func(next *GitContribution) { next.IntegrationSHA = strings.Repeat("0", 40) },
		"integration-width": func(next *GitContribution) { next.IntegrationSHA = strings.Repeat("b", 64) },
		"integration-short": func(next *GitContribution) { next.IntegrationSHA = strings.Repeat("b", 7) },
		"destination":       func(next *GitContribution) { next.DestinationBranchID = "" },
		"destination-size":  func(next *GitContribution) { next.DestinationBranchID = strings.Repeat("a", 129) },
		"source":            func(next *GitContribution) { next.SourceBranchID = "\ninvalid" },
		"source-size":       func(next *GitContribution) { next.SourceBranchID = strings.Repeat("a", 129) },
		"original-head":     func(next *GitContribution) { next.OriginalHead = strings.Repeat("g", 40) },
		"original-width":    func(next *GitContribution) { next.OriginalHead = strings.Repeat("a", 64) },
		"original-zero":     func(next *GitContribution) { next.OriginalHead = strings.Repeat("0", 40) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			contribution := gitContributionFixture()
			mutate(&contribution)
			if err := contribution.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatal("invalid contribution accepted", err)
			}
		})
	}
	for _, id := range []string{"", strings.Repeat("a", 64), strings.ToUpper(gitContributionFixture().ID), "../outside"} {
		contribution := gitContributionFixture()
		contribution.ID = id
		if !errors.Is(contribution.Validate(), ErrValidation) {
			t.Fatal("invalid key accepted", id)
		}
	}
	contribution := gitContributionFixture()
	contribution.IntegrationSHA = strings.Repeat("b", 64)
	contribution.OriginalHead = strings.Repeat("c", 64)
	if err := contribution.WithID().Validate(); err != nil {
		t.Fatal("SHA-256 Git IDs rejected", err)
	}
}

func TestGitContributionMemoryIdentityAndValidation(t *testing.T) {
	contribution := gitContributionFixture()
	memory := GitContributionMemory{RepoID: contribution.RepoID, ContributionID: contribution.ID, PublicationID: strings.Repeat("d", 32), MemoryHash: HashContent([]byte("memory-v1")), MemorySource: contribution.Capture, MemoryPinned: true}.WithID()
	if memory.Validate() != nil || len(memory.ID) != 64 || memory.WithID() != memory {
		t.Fatal("invalid memory fixture", memory)
	}
	if memory.ID != "dc0d22eedd37810f257805f6b85747d3ad37cfdaf8937b637abd053c3dd6728b" {
		t.Fatal("version-1 memory encoding changed", memory.ID)
	}
	mutations := map[string]func(*GitContributionMemory){
		"repo":         func(next *GitContributionMemory) { next.RepoID = HashContent([]byte("other repo")) },
		"contribution": func(next *GitContributionMemory) { next.ContributionID = strings.Repeat("e", 64) },
		"publication":  func(next *GitContributionMemory) { next.PublicationID = strings.Repeat("f", 32) },
		"memory":       func(next *GitContributionMemory) { next.MemoryHash = HashContent([]byte("memory-v2")) },
		"source":       func(next *GitContributionMemory) { next.MemorySource = HashContent([]byte("other source")) },
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
	invalid := []GitContributionMemory{memory, memory, memory, memory, memory, memory, memory, memory}
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
