package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGitReversalUsesExactChangesAndPreservesUnrelatedWork(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 40) }
	entry := func(c string) GitEntry { return GitEntry{OID: oid(c), Mode: "100644"} }
	a := GitCommitDelta{Commit: oid("a"), Parents: []string{oid("f")}, Parent: oid("f"), Complete: true, Changes: []GitPathChange{{Path: "login.go", Before: entry("1"), After: entry("2")}, {Path: "login_test.go", Before: GitEntry{}, After: entry("3")}}}
	// B is the actual comparison parent. Its independent file does not appear in
	// this revert and therefore can never be included in its cancellation scope.
	r := GitCommitDelta{Commit: oid("c"), Parents: []string{oid("b")}, Parent: oid("b"), Complete: true, Changes: []GitPathChange{{Path: "login.go", Before: entry("2"), After: entry("1")}, {Path: "login_test.go", Before: entry("3"), After: GitEntry{}}}}
	proof, err := AssessGitReversal(a, r)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Coverage != "full" || !reflect.DeepEqual(proof.Paths, []string{"login.go", "login_test.go"}) {
		t.Fatalf("reversal=%+v", proof)
	}
	partial := r
	partial.Changes = r.Changes[:1]
	proof, err = AssessGitReversal(a, partial)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Coverage != "partial" || !reflect.DeepEqual(proof.UnverifiedPaths, []string{"login_test.go"}) {
		t.Fatalf("partial=%+v", proof)
	}
	rr := GitCommitDelta{Commit: oid("d"), Parents: []string{r.Commit}, Parent: r.Commit, Complete: true, Changes: a.Changes}
	proof, err = AssessGitReversal(r, rr)
	if err != nil || proof.Coverage != "full" || proof.Target != r.Commit {
		t.Fatalf("reapplication proof=%+v %v", proof, err)
	}
	// A repeated default message does not matter; a file with intervening edits
	// cannot be canceled on behalf of the original commit without stronger proof.
	modified := r
	modified.Changes = append([]GitPathChange(nil), r.Changes...)
	modified.Changes[0].Before = entry("4")
	proof, err = AssessGitReversal(a, modified)
	if err != nil || proof.Coverage != "partial" || !reflect.DeepEqual(proof.Paths, []string{"login_test.go"}) {
		t.Fatalf("intervening edits=%+v %v", proof, err)
	}
	incomplete := r
	incomplete.Complete = false
	proof, err = AssessGitReversal(a, incomplete)
	if err != nil || proof.Coverage != "unverified" || len(proof.Paths) != 0 {
		t.Fatalf("truncated evidence=%+v %v", proof, err)
	}
	mixed := r
	mixed.Changes = append(append([]GitPathChange(nil), r.Changes...), GitPathChange{Path: "reason.md", After: entry("5")})
	proof, err = AssessGitReversal(a, mixed)
	if err != nil || proof.Coverage != "full" || !reflect.DeepEqual(proof.OtherPaths, []string{"reason.md"}) {
		t.Fatalf("independent new work=%+v %v", proof, err)
	}
}

func TestGitReversalRequiresMergeMainlineAndExactModes(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 64) }
	a := GitCommitDelta{Commit: oid("a"), Parents: []string{oid("b"), oid("c")}, Complete: true, Changes: []GitPathChange{{Path: "tool", Before: GitEntry{oid("1"), "100644"}, After: GitEntry{oid("1"), "100755"}}}}
	r := GitCommitDelta{Commit: oid("d"), Parents: []string{a.Commit}, Parent: a.Commit, Complete: true, Changes: []GitPathChange{{Path: "tool", Before: GitEntry{oid("1"), "100755"}, After: GitEntry{oid("1"), "100644"}}}}
	if _, err := AssessGitReversal(a, r); !errors.Is(err, ErrValidation) {
		t.Fatal("merge mainline guessed", err)
	}
	a.Parent = a.Parents[0]
	proof, err := AssessGitReversal(a, r)
	if err != nil || proof.Coverage != "full" {
		t.Fatalf("mode-only reversal=%+v %v", proof, err)
	}
	r.Changes = append(r.Changes, r.Changes[0])
	if _, err := AssessGitReversal(a, r); !errors.Is(err, ErrValidation) {
		t.Fatal("duplicate paths accepted", err)
	}
}
