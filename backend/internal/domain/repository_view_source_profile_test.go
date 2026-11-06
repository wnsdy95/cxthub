package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRepositoryGraphProjectionSourceProfile(t *testing.T) {
	checkpoint := RepositoryViewSourceCheckpoint{Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
		RepoID: HashContent([]byte(t.Name())), Epoch: "12345678-1234-1234-1234-123456789abc", Sequence: 7}
	for _, profile := range []string{"", RepositoryGraphProjectionSourceProfile} {
		request := RepositoryViewSourceRequest{Version: RepositoryViewSourceVersion, Profile: profile, After: &checkpoint}
		if err := request.Validate(); err != nil {
			t.Fatalf("valid source profile %q rejected: %v", profile, err)
		}
		for _, value := range []any{request, RepositoryViewSourcePage{Profile: profile, Checkpoint: &checkpoint}} {
			raw, err := json.Marshal(value)
			if err != nil || strings.Contains(string(raw), `"profile":`) != (profile != "") {
				t.Fatalf("source profile lost its default wire compatibility: %s %v", raw, err)
			}
		}
	}
	if err := (RepositoryViewSourceRequest{Version: RepositoryViewSourceVersion, Profile: "unknown"}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown source profile accepted: %v", err)
	}
	checkpoint.Scope = RepositoryViewSourceScope
	if err := (RepositoryViewSourceRequest{Version: RepositoryViewSourceVersion, Profile: RepositoryGraphProjectionSourceProfile, After: &checkpoint}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("graph profile accepted a metadata-only checkpoint: %v", err)
	}
	for _, kind := range []string{"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision",
		"git_commit", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution", "git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof"} {
		if !RepositoryGraphProjectionSourceKindValid(kind) || !RepositoryGraphSourceKindValid(kind) {
			t.Fatalf("projection profile lost a dependency: %s", kind)
		}
	}
	for _, kind := range []string{"git_tree", "git_delta"} {
		if RepositoryGraphProjectionSourceKindValid(kind) || !RepositoryGraphSourceKindValid(kind) {
			t.Fatalf("projection filtering changed the full source contract: %s", kind)
		}
	}
	for _, kind := range []string{"", "secrets", "unknown"} {
		if RepositoryGraphProjectionSourceKindValid(kind) {
			t.Fatalf("projection profile accepted an unknown kind: %s", kind)
		}
	}
}
