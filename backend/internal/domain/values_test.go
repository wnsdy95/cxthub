package domain

import (
	"errors"
	"strings"
	"testing"
)

func validHashForTest(c string) ContentHash {
	return ContentHash("sha256:" + strings.Repeat(c, 64))
}

func TestValidateContentHash(t *testing.T) {
	valid := validHashForTest("a")
	if err := ValidateContentHash(valid); err != nil {
		t.Fatalf("valid hash rejected: %v", err)
	}
	for _, h := range []ContentHash{
		"",
		"sha256:deadbeef",
		ContentHash("sha256:" + strings.Repeat("A", 64)),
		"sha256:../../outside",
		ContentHash(strings.Repeat("a", 64)),
	} {
		if err := ValidateContentHash(h); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("expected ErrIntegrity for %q, got %v", h, err)
		}
	}
}

func TestValidateRefName(t *testing.T) {
	for _, name := range []string{"main", "feature/auth-flow", "pr-scenario-0"} {
		if err := ValidateRefName(RefBranch, name); err != nil {
			t.Fatalf("valid ref name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"../main", "/main", "main/", "-option", "feature//x", "a..b", ".hidden", "topic.lock", "bad\\name", "bad name", "bad@{name"} {
		if err := ValidateRefName(RefBranch, name); !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation for %q, got %v", name, err)
		}
	}
	if err := ValidateRefName(RefHead, HeadRefName); err != nil {
		t.Fatalf("HEAD name rejected: %v", err)
	}
	if err := ValidateRefName(RefHead, "main"); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for non-HEAD name, got %v", err)
	}
}

func TestValidateRef(t *testing.T) {
	repo := validHashForTest("0")
	target := validHashForTest("1")
	if err := ValidateRef(Ref{Kind: RefBranch, Name: "main", RepoID: repo, Target: target}); err != nil {
		t.Fatalf("valid branch ref rejected: %v", err)
	}
	if err := ValidateRef(Ref{Kind: RefSession, Name: "fork/abc", RepoID: repo, Target: target}); err != nil {
		t.Fatalf("valid session ref rejected: %v", err)
	}
	if err := ValidateRef(Ref{Kind: RefHead, Name: HeadRefName, RepoID: repo, Symbolic: "refs/heads/main"}); err != nil {
		t.Fatalf("valid symbolic HEAD rejected: %v", err)
	}
	if err := ValidateRef(Ref{Kind: RefBranch, Name: "../main", RepoID: repo, Target: target}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for bad ref name, got %v", err)
	}
	if err := ValidateRef(Ref{Kind: RefBranch, Name: "main", RepoID: "sha256:../../outside", Target: target}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for bad repo hash, got %v", err)
	}
	if err := ValidateRef(Ref{Kind: RefBranch, Name: "main", RepoID: repo, Target: "sha256:../../outside"}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity for bad target hash, got %v", err)
	}
}

func TestSessionRefPrefixSeparatesNestedGitBranches(t *testing.T) {
	feature := SessionRefPrefix("feature")
	nested := SessionRefPrefix("feature/foo")
	if strings.HasPrefix(nested, feature) || strings.HasPrefix(feature, nested) {
		t.Fatalf("session ref scopes overlap: feature=%q nested=%q", feature, nested)
	}
	if got, want := SessionRefPrefix("ééé"), "fork/v1/6/ééé/"; got != want {
		t.Fatalf("UTF-8 byte length prefix = %q, want %q", got, want)
	}
}
