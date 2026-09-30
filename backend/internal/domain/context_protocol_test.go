package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestContextRefWriteNeedsState(t *testing.T) {
	for _, protocol := range []int{0, 1, 2, -1} {
		for _, kind := range []RefKind{RefBranch, RefHead, RefTag, RefSession} {
			for _, identity := range []string{"", "branch-id"} {
				t.Run(fmt.Sprintf("%d/%s/%s", protocol, kind, identity), func(t *testing.T) {
					next := Ref{Kind: kind, BranchID: identity}
					wantErr := protocol != 0 && protocol != 1 || protocol == 1 && kind == RefBranch && identity == ""
					wantState := protocol == 1 && kind == RefBranch && identity != ""
					needsState, err := ContextRefWriteNeedsState(protocol, next)
					if needsState != wantState || (err != nil) != wantErr || err != nil && !errors.Is(err, ErrConflict) {
						t.Fatalf("needsState=%v err=%v; want state=%v conflict=%v", needsState, err, wantState, wantErr)
					}
					if !needsState {
						// The full validator must enforce the same preflight even when
						// supplied a history that cannot be projected.
						bad := []HistoryEvent{{Kind: "rename", BindingParent: "missing"}}
						got := ValidateContextRefWrite(protocol, bad, nil, next)
						if (got != nil) != wantErr || got != nil && !errors.Is(got, ErrConflict) {
							t.Fatalf("full validator disagrees: %v", got)
						}
					}
				})
			}
		}
	}
}

func TestContextRefWriteBranchState(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	next := Ref{RepoID: repo, Kind: RefBranch, Name: "work", BranchID: "second", Target: HashContent([]byte("same target"))}
	birth := HistoryEvent{ID: "birth", RepoID: string(repo), Kind: "birth", Branch: "work", BranchID: "first"}
	rename := HistoryEvent{ID: "rename", RepoID: string(repo), Kind: "rename", PreviousBranch: "work", Branch: "moved", BranchID: "first", BindingParent: birth.ID}
	reuse := HistoryEvent{ID: "reuse", RepoID: string(repo), Kind: "birth", Branch: "work", BranchID: "second", BindingParent: rename.ID}
	history := []HistoryEvent{birth, rename, reuse}
	if err := ValidateContextRefWrite(1, history, &next, next); err != nil {
		t.Fatal(err)
	}
	stale := next
	stale.BranchID = "first"
	if err := ValidateContextRefWrite(1, history, &next, stale); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("same-hash name reuse accepted: %v", err)
	}
	if err := ValidateContextRefWrite(1, history, &stale, next); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("mismatched current identity accepted: %v", err)
	}
	legacy := next
	legacy.BranchID = LegacyContextBranchID(string(repo), next.Name)
	if err := ValidateContextRefWrite(1, nil, &legacy, legacy); err != nil {
		t.Fatal("migrated legacy pointer", err)
	}
	if err := ValidateContextRefWrite(1, nil, nil, legacy); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("missing durable birth accepted: %v", err)
	}
	if err := ValidateContextRefWrite(1, history[:2], &legacy, legacy); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("released name revived: %v", err)
	}
	if err := ValidateContextRefWrite(1, []HistoryEvent{rename}, &next, next); err == nil {
		t.Fatal("incomplete history accepted")
	}
}
