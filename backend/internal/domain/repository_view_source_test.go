package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRepositoryViewSourceCheckpointContract(t *testing.T) {
	checkpoint := RepositoryViewSourceCheckpoint{Version: 1, Scope: RepositoryViewSourceScope, RepoID: HashContent([]byte(t.Name())), Epoch: "12345678-1234-1234-1234-123456789abc", Sequence: 9007199254740993}
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil || !strings.Contains(string(encoded), `"sequence":"9007199254740993"`) {
		t.Fatalf("unsafe integer encoding: %s %v", encoded, err)
	}
	for _, test := range []struct {
		name   string
		change func(*RepositoryViewSourceCheckpoint)
		want   error
	}{
		{"scope", func(value *RepositoryViewSourceCheckpoint) { value.Scope = CatalogScope }, ErrCatalogResetRequired},
		{"version", func(value *RepositoryViewSourceCheckpoint) { value.Version++ }, ErrCatalogResetRequired},
		{"repo", func(value *RepositoryViewSourceCheckpoint) { value.RepoID = "bad" }, ErrValidation},
		{"epoch", func(value *RepositoryViewSourceCheckpoint) { value.Epoch = "bad" }, ErrValidation},
		{"zero_epoch", func(value *RepositoryViewSourceCheckpoint) { value.Epoch = "00000000-0000-0000-0000-000000000000" }, ErrValidation},
		{"negative", func(value *RepositoryViewSourceCheckpoint) { value.Sequence = -1 }, ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := checkpoint
			test.change(&value)
			if err := value.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
	page := RepositoryViewSourcePage{Through: checkpoint.Sequence, Entries: []RepositoryViewSourceEntry{{Sequence: checkpoint.Sequence}}}
	encoded, err = json.Marshal(page)
	if err != nil || !strings.Contains(string(encoded), `"through":"9007199254740993"`) || !strings.Contains(string(encoded), `"sequence":"9007199254740993"`) {
		t.Fatalf("unsafe page encoding: %s %v", encoded, err)
	}
}

func TestRepositoryViewSourceRequestContract(t *testing.T) {
	for _, request := range []RepositoryViewSourceRequest{
		{Version: 1}, {Version: 1, Limit: MaxRepositoryViewSourceLimit}, {Version: 1, Cursor: "adapter-owned"},
	} {
		if err := request.Validate(); err != nil {
			t.Fatalf("valid request %+v: %v", request, err)
		}
	}
	for _, request := range []RepositoryViewSourceRequest{
		{}, {Version: 2}, {Version: 1, Limit: -1}, {Version: 1, Limit: MaxRepositoryViewSourceLimit + 1},
		{Version: 1, Cursor: strings.Repeat("a", 16<<10+1)}, {Version: 1, Cursor: "cursor", After: &RepositoryViewSourceCheckpoint{}},
	} {
		if err := request.Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid request %+v: %v", request, err)
		}
	}
	for _, kind := range []string{"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision"} {
		if !RepositoryViewSourceKindValid(kind) {
			t.Fatalf("unsupported source kind %s", kind)
		}
	}
	for _, kind := range []string{"", "secrets", "doc", "memory_body", "github_token"} {
		if RepositoryViewSourceKindValid(kind) {
			t.Fatalf("unsafe source kind %s", kind)
		}
	}
}

func TestRepositoryGraphSourceScopeRemainsPrivate(t *testing.T) {
	checkpoint := RepositoryViewSourceCheckpoint{Version: 1, Scope: RepositoryGraphSourceScope, RepoID: HashContent([]byte(t.Name())), Epoch: "12345678-1234-1234-1234-123456789abc", Sequence: 1}
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RepositoryViewSourceRequest{Version: 1, After: &checkpoint}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"git_commit", "git_tree", "git_delta", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution", "git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof"} {
		if !RepositoryGraphSourceKindValid(kind) || RepositoryViewSourceKindValid(kind) {
			t.Fatalf("source kind %s escaped its private graph scope", kind)
		}
	}
	for _, kind := range []string{"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision"} {
		if !RepositoryGraphSourceKindValid(kind) {
			t.Fatalf("graph source lost shared kind %s", kind)
		}
	}
	for _, kind := range []string{"", "secrets", "doc", "memory_body", "memory_grant", "github_token", "git_scan_job"} {
		if RepositoryGraphSourceKindValid(kind) {
			t.Fatalf("unsupported graph source kind %s", kind)
		}
	}
}
