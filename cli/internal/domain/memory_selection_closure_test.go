package domain

import (
	"reflect"
	"testing"
)

func TestMemorySelectionParentTrackingAndPublicationClosure(t *testing.T) {
	before, after := memorySelectionPair()
	ordered, err := OrderHistoryEvents([]HistoryEvent{after, before})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := trackingHistoryClosure(ordered, map[string]bool{after.ID: true})
	if err != nil || !reflect.DeepEqual(proof, []HistoryEvent{before, after}) {
		t.Fatalf("tracking closure lost predecessor: %v %v", proof, err)
	}
	ref := Ref{RepoID: before.RepoID, Kind: RefBranch, Name: before.Branch, BranchID: LegacyContextBranchID(before.RepoID, before.Branch), Target: before.Target}
	before.BranchID, after.BranchID = ref.BranchID, ref.BranchID
	in := PublicationPlanInput{RepoID: before.RepoID, ContextProtocol: 1, Scope: PublicationScope{Branches: []PublicationBranch{{Branch: ref.Name, BranchID: ref.BranchID}}}, Refs: []Ref{ref}, History: []HistoryEvent{after, before}}
	got, err := PlanPublication(in)
	if err != nil || !reflect.DeepEqual(got.HistoryToSend, []HistoryEvent{before, after}) {
		t.Fatalf("publication order lost predecessor: %v %v", got.HistoryToSend, err)
	}
	in.Accepted = []HistoryEvent{before}
	got, err = PlanPublication(in)
	if err != nil || !reflect.DeepEqual(got.HistoryToSend, []HistoryEvent{after}) {
		t.Fatalf("accepted predecessor was resent: %v %v", got.HistoryToSend, err)
	}
	in.Accepted = nil
	in.History = []HistoryEvent{after}
	if _, err = PlanPublication(in); err == nil {
		t.Fatal("scoped publication omitted required predecessor")
	}
}

func TestMemorySelectionParentTrackingProofRemainsImmutable(t *testing.T) {
	before, after := memorySelectionPair()
	before.BranchID = LegacyContextBranchID(before.RepoID, before.Branch)
	after.BranchID = before.BranchID
	ref := Ref{RepoID: before.RepoID, Kind: RefBranch, Name: before.Branch, BranchID: before.BranchID, Target: before.Target}
	proof, err := TrackingProof(ref, []HistoryEvent{after, before}, after.GitAfter, after.Target)
	if err != nil || !reflect.DeepEqual(proof, []HistoryEvent{before, after}) {
		t.Fatalf("raw proof changed: %v %v", proof, err)
	}
	a := TrackingAttachment{Event: HistoryEvent{BranchID: after.BranchID}, Proof: proof}
	applied, err := AppliedTrackingProof(a)
	if err != nil || !reflect.DeepEqual(applied, proof) {
		t.Fatalf("apply lost memory edge: %v %v", applied, err)
	}
}
