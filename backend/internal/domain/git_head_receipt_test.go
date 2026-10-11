package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func gitHeadReceiptFixture() (GitHeadScan, []GitRefObservation) {
	claim := GitHeadScan{RepoID: HashContent([]byte("head receipt")), GitOrigin: "https://github.com/acme/api.git", Version: 1, Page: 1, State: "running", LeaseUntil: time.Unix(500, 0).UTC()}
	observations := []GitRefObservation{}
	for _, branch := range []string{"z-last", "a-first"} {
		observations = append(observations, GitRefObservation{RepoID: claim.RepoID, GitOrigin: claim.GitOrigin, Source: "reconciliation", Ref: "refs/heads/" + branch, After: strings.Repeat("a", 40), Provider: &GitProviderObservation{InstallationID: 789, RepositoryID: 91}}.WithID())
	}
	return claim, observations
}

func TestGitHeadReceiptCanonicalOccurrenceIdentity(t *testing.T) {
	claim, observations := gitHeadReceiptFixture()
	receipt, err := NewGitHeadReceipt(claim, observations, true)
	if err != nil || receipt == nil || receipt.Validate() != nil || len(receipt.ID) != 64 || receipt.Heads[0].Ref != observations[1].Ref || !receipt.More {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	if observations[0].Ref != "refs/heads/z-last" {
		t.Fatal("constructor sorted caller-owned observations")
	}
	observations[0], observations[1] = observations[1], observations[0]
	reordered, err := NewGitHeadReceipt(claim, observations, true)
	if err != nil || !reflect.DeepEqual(receipt, reordered) {
		t.Fatal("provider order changed occurrence", reordered, err)
	}
	claim.Version++
	repeated, err := NewGitHeadReceipt(claim, observations, true)
	if err != nil || receipt.ID == repeated.ID || !reflect.DeepEqual(receipt.Heads, repeated.Heads) {
		t.Fatal("new occurrence was deduplicated", repeated, err)
	}
	changed, err := NewGitHeadReceipt(claim, observations, false)
	if err != nil || changed.ID == repeated.ID {
		t.Fatal("more flag not immutable", err)
	}
	raw, err := json.Marshal(receipt)
	var restored GitHeadReceipt
	if err != nil || json.Unmarshal(raw, &restored) != nil || !reflect.DeepEqual(receipt, &restored) || restored.Validate() != nil {
		t.Fatal("receipt JSON roundtrip failed")
	}
}

func TestGitHeadReceiptRejectsMixedAndMalformedPages(t *testing.T) {
	for _, scenario := range []string{"duplicate-ref", "mixed-provider", "mixed-qualification", "foreign-repo", "foreign-origin", "push", "before", "empty-head", "tampered-id", "too-many", "not-running"} {
		t.Run(scenario, func(t *testing.T) {
			claim, observations := gitHeadReceiptFixture()
			switch scenario {
			case "duplicate-ref":
				observations[1] = observations[0]
			case "mixed-provider":
				observations[1].Provider.RepositoryID++
			case "mixed-qualification":
				observations[1].Provider = nil
			case "foreign-repo":
				observations[1].RepoID = HashContent([]byte("foreign"))
			case "foreign-origin":
				observations[1].GitOrigin += "-other"
			case "push":
				observations[1].Source, observations[1].Delivery = "github-push", "delivery"
			case "before":
				observations[1].Before = strings.Repeat("b", 40)
			case "empty-head":
				observations[1].Before, observations[1].After = observations[1].After, ""
			case "too-many":
				observations = make([]GitRefObservation, 101)
			case "not-running":
				claim.State = "completed"
			}
			observations[1] = observations[1].WithID()
			if scenario == "tampered-id" {
				observations[1].ID = strings.Repeat("b", 64)
			}
			if receipt, err := NewGitHeadReceipt(claim, observations, false); err == nil || receipt != nil {
				t.Fatal("invalid page produced a receipt", receipt, err)
			}
		})
	}
	claim, observations := gitHeadReceiptFixture()
	receipt, _ := NewGitHeadReceipt(claim, observations, false)
	receipt.Heads[0], receipt.Heads[1] = receipt.Heads[1], receipt.Heads[0]
	if receipt.WithID().Validate() == nil {
		t.Fatal("noncanonical persisted receipt accepted")
	}
}

func TestGitHeadReceiptLegacyAndEmptyRemainUnqualified(t *testing.T) {
	claim, observations := gitHeadReceiptFixture()
	for index := range observations {
		observations[index].Provider = nil
		observations[index] = observations[index].WithID()
	}
	for _, page := range [][]GitRefObservation{nil, {}, observations} {
		if receipt, err := NewGitHeadReceipt(claim, page, false); err != nil || receipt != nil {
			t.Fatal("legacy or empty page asserted provider authority", receipt, err)
		}
	}
}
