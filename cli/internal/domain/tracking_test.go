package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrackingHistoryRequiresCompatibleObservedIdentity(t *testing.T) {
	repo := string(HashContent([]byte("tracking repository")))
	target := HashContent([]byte("context"))
	ref := Ref{RepoID: repo, Kind: RefBranch, Name: "task", BranchID: "task-identity", Target: target}
	birth := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, Branch: "task", BranchID: ref.BranchID, Kind: "birth", Target: target, GitAfter: strings.Repeat("1", 40), MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
	observation := birth
	observation.ID, observation.Kind, observation.Source = strings.Repeat("b", 32), "position", target
	observation.MemoryHash = HashContent([]byte("recorded memory"))
	observation.CreatedAt = time.Unix(2, 0).UTC()
	for _, mode := range []string{"same identity", "remote only birth", "ref identity differs", "missing remote birth", "conflicting ordinary ID", "competing local birth", "local archive", "local memory differs", "local unpublished target", "identical local pin", "unrelated local work", "legacy", "released legacy", "name reused with proof", "name reused with unapplied proof"} {
		t.Run(mode, func(t *testing.T) {
			r := ref
			remote := []HistoryEvent{birth, observation}
			local := []HistoryEvent{birth}
			wantError := true
			switch mode {
			case "same identity":
				wantError = false
			case "remote only birth":
				local = nil
				wantError = false
			case "ref identity differs":
				r.BranchID = "different"
			case "missing remote birth":
				remote = remote[1:]
			case "conflicting ordinary ID":
				bad := observation
				bad.MemoryHash = ""
				local = append(local, bad)
			case "competing local birth":
				local[0].ID = strings.Repeat("c", 32)
				local[0].BranchID = "different"
			case "local archive":
				e := birth
				e.ID, e.Kind, e.BindingParent = strings.Repeat("c", 32), "archive", birth.ID
				local = append(local, e)
			case "local memory differs", "local unpublished target", "identical local pin":
				e := observation
				e.ID = strings.Repeat("c", 32)
				if mode == "local memory differs" {
					e.MemoryHash = HashContent([]byte("unpublished"))
				}
				if mode == "local unpublished target" {
					e.Target = HashContent([]byte("unpublished"))
				}
				if mode == "identical local pin" {
					wantError = false
				}
				local = append(local, e)
			case "unrelated local work":
				e := observation
				e.ID, e.BranchID, e.Branch = strings.Repeat("c", 32), "unrelated", "other"
				local = append(local, e)
				wantError = false
			case "legacy":
				r.BranchID = ""
				ob := observation
				ob.BranchID = LegacyContextBranchID(repo, r.Name)
				remote = []HistoryEvent{ob}
				local = nil
				wantError = false
			case "released legacy":
				r.BranchID = ""
				e := birth
				e.BranchID = LegacyContextBranchID(repo, r.Name)
				e.Kind = "archive"
				remote = []HistoryEvent{e}
				local = nil
			case "name reused with proof", "name reused with unapplied proof":
				wantError = false
				archive := birth
				archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("c", 32), "archive", birth.ID
				again := birth
				again.ID, again.BranchID, again.BindingParent = strings.Repeat("d", 32), "new-identity", archive.ID
				r.BranchID = again.BranchID
				remote = []HistoryEvent{again, archive, birth}
				local = []HistoryEvent{birth, archive}
				if mode == "name reused with proof" {
					local = append(local, again)
					wantError = false
				}
			}
			beforeRemote := append([]HistoryEvent{}, remote...)
			beforeLocal := append([]HistoryEvent{}, local...)
			got, err := TrackingHistory(r, remote, local)
			if wantError && !errors.Is(err, ErrSyncConflict) {
				t.Fatalf("got %+v, %v; want conflict", got, err)
			}
			if !wantError && (err != nil || len(got) != len(remote)) {
				t.Fatalf("got %+v, %v", got, err)
			}
			if !reflect.DeepEqual(append([]HistoryEvent{}, remote...), beforeRemote) || !reflect.DeepEqual(append([]HistoryEvent{}, local...), beforeLocal) {
				t.Fatal("query mutated evidence")
			}
		})
	}
}

func TestTrackingProofKeepsCausalDependenciesWithoutAdoptingOtherBranches(t *testing.T) {
	repo := string(HashContent([]byte("closure")))
	target := HashContent([]byte("same conversation"))
	code := strings.Repeat("1", 40)
	old := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "old", Branch: "task", Kind: "birth", Target: target, MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
	archive := old
	archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("b", 32), "archive", old.ID
	birth := old
	birth.ID, birth.BranchID, birth.BindingParent = strings.Repeat("c", 32), "current", archive.ID
	birth.GitAfter = code
	unrelated := old
	unrelated.ID, unrelated.BranchID, unrelated.Branch = strings.Repeat("d", 32), "other", "other"
	ref := Ref{RepoID: repo, Kind: RefBranch, Name: "task", BranchID: birth.BranchID, Target: target}
	proof, err := TrackingProof(ref, []HistoryEvent{unrelated, birth, archive, old}, code, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 3 || proof[0].ID != old.ID || proof[1].ID != archive.ID || proof[2].ID != birth.ID {
		t.Fatalf("wrong closure: %+v", proof)
	}
	e := birth
	e.ID, e.Kind, e.Source, e.SharedTarget, e.BindingParent = strings.Repeat("e", 32), "attach", target, target, ""
	a := TrackingAttachment{Event: e, ObservedRef: ref, Proof: proof, Code: code}
	if err := ValidateTrackingAttachment(a); err != nil {
		t.Fatal(err)
	}
	applied, err := AppliedTrackingProof(a)
	if err != nil || !reflect.DeepEqual(applied, proof) {
		t.Fatalf("application lost required cross-identity dependencies: %+v, %v", applied, err)
	}
	a.Proof = append(a.Proof, unrelated)
	if !errors.Is(ValidateTrackingAttachment(a), ErrHashMismatch) {
		t.Fatal("unrelated history accepted for application")
	}
}

func TestTrackingAttachmentRequiresRecordedPinAndCode(t *testing.T) {
	repo := string(HashContent([]byte("pin proof")))
	target := HashContent([]byte("context"))
	code := strings.Repeat("1", 40)
	birth := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "task-id", Branch: "task", Kind: "birth", Source: target, Target: target, GitAfter: code, MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
	e := birth
	e.ID, e.Kind, e.SharedTarget = strings.Repeat("b", 32), "attach", target
	a := TrackingAttachment{Event: e, ObservedRef: Ref{RepoID: repo, Kind: RefBranch, Name: "task", BranchID: birth.BranchID, Target: target}, Proof: []HistoryEvent{birth}, Code: code}
	if err := ValidateTrackingAttachment(a); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"code", "memory", "unpinned", "identity", "shared target"} {
		t.Run(mode, func(t *testing.T) {
			bad := a
			switch mode {
			case "code":
				bad.Code = strings.Repeat("2", 40)
			case "memory":
				bad.Event.MemoryHash = HashContent([]byte("unrecorded"))
			case "unpinned":
				bad.Event.MemoryPinned = false
			case "identity":
				bad.Event.BranchID = "unrelated"
			case "shared target":
				bad.Event.SharedTarget = HashContent([]byte("unrelated"))
			}
			if err := ValidateTrackingAttachment(bad); err == nil {
				t.Fatal("unproven attachment accepted")
			}
		})
	}
}
