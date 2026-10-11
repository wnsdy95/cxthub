package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGitHeadSourceReceiptKeepsReadKindsSeparate(t *testing.T) {
	claim, observations := gitHeadReceiptFixture()
	page, sources := observations[:1], observations[1:]
	receipt, err := NewGitHeadReceiptWithSources(claim, page, sources, false)
	if err != nil || receipt == nil || receipt.Validate() != nil || len(receipt.Heads) != 1 || len(receipt.PriorSourceHeads) != 1 {
		t.Fatal(receipt, err)
	}
	if receipt.Heads[0].Ref != page[0].Ref || receipt.PriorSourceHeads[0].Ref != sources[0].Ref {
		t.Fatal("source read masqueraded as a page member")
	}
	head, found := receipt.SourceHead(sources[0].Ref)
	if !found || head.ObservationID != sources[0].ID {
		t.Fatal("qualified source was not available to admission")
	}
	legacy, err := NewGitHeadReceipt(claim, page, false)
	if err != nil || legacy.ID == receipt.ID {
		t.Fatal("source proof did not affect receipt identity")
	}
	withoutSources, err := NewGitHeadReceiptWithSources(claim, page, nil, false)
	if err != nil || !reflect.DeepEqual(legacy, withoutSources) {
		t.Fatal("legacy receipt changed without source reads")
	}
	raw, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(raw), "prior_source_heads") {
		t.Fatal("legacy wire format gained an empty field")
	}
	current := sources[0]
	current.After = strings.Repeat("b", 40)
	current = current.WithID()
	receipt, err = NewGitHeadReceiptWithSources(claim, []GitRefObservation{current}, sources, false)
	if err != nil {
		t.Fatal(err)
	}
	head, found = receipt.SourceHead(current.Ref)
	if !found || head.Commit != current.After {
		t.Fatal("earlier source read overrode later page head")
	}
	if empty, err := NewGitHeadReceiptWithSources(claim, nil, sources, false); err != nil || empty != nil {
		t.Fatal("empty page supplied destination evidence", empty, err)
	}
}

func TestGitHeadSourceReceiptRejectsUnqualifiedOrMalformedProof(t *testing.T) {
	for _, scenario := range []string{"providerless-source", "providerless-page", "different-provider", "different-repo", "duplicate", "too-many"} {
		t.Run(scenario, func(t *testing.T) {
			claim, observations := gitHeadReceiptFixture()
			page, sources := observations[:1], observations[1:]
			switch scenario {
			case "providerless-source":
				sources[0].Provider = nil
			case "providerless-page":
				page[0].Provider = nil
			case "different-provider":
				sources[0].Provider.RepositoryID++
			case "different-repo":
				sources[0].RepoID = HashContent([]byte("foreign"))
			case "duplicate":
				sources = append(sources, sources[0])
			case "too-many":
				sources = make([]GitRefObservation, MaxGitHeadSourceReads+1)
			}
			page[0], sources[0] = page[0].WithID(), sources[0].WithID()
			if receipt, err := NewGitHeadReceiptWithSources(claim, page, sources, false); err == nil || receipt != nil {
				t.Fatal("invalid source qualified", receipt, err)
			}
		})
	}
}

func TestGitAdmissionUsesPriorSourceButNeverPriorDestination(t *testing.T) {
	admission, receipt := gitAdmissionFixture(t)
	var source, destination GitHeadReceiptHead
	for _, head := range receipt.Heads {
		if head.Ref == "refs/heads/"+admission.SourceBranch {
			source = head
		} else {
			destination = head
		}
	}
	receipt.Heads, receipt.PriorSourceHeads = []GitHeadReceiptHead{destination}, []GitHeadReceiptHead{source}
	receipt = receipt.WithID()
	admission.Destination.ReadReceiptID = receipt.ID
	admission.Destination = admission.Destination.WithID()
	admission = admission.WithID()
	if !admission.MatchesBaseline(receipt) {
		t.Fatal("ordered source and destination reads did not qualify")
	}
	receipt.Heads, receipt.PriorSourceHeads = receipt.PriorSourceHeads, receipt.Heads
	receipt = receipt.WithID()
	admission.Destination.ReadReceiptID = receipt.ID
	admission.Destination = admission.Destination.WithID()
	admission = admission.WithID()
	if admission.MatchesBaseline(receipt) {
		t.Fatal("older source-only proof became a destination baseline")
	}
}
