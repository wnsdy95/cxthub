package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDecodeRepositoryViewSourcePreservesPendingAndLeavesSemanticsForProjection(t *testing.T) {
	for _, protocol := range []int{0, 1} {
		repo, entries, want := pendingProjectionFixture(t, protocol)
		supported := true
		for index := range entries {
			if entries[index].Kind == "repository" {
				entries[index].Value = pendingProjectionTestJSON(t, pendingProjectionRepository{Repo: Repo{ID: repo, DefaultBranch: "main", ContextProtocol: protocol, GitRemoteURL: "https://github.com/owner/repo.git"}, RemoteURLSupported: &supported, GitRemoteURLSupported: &supported})
			}
		}
		metadata, view, err := DecodeRepositoryViewSource(context.Background(), repo, entries)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.ID != repo || metadata.ContextProtocol != protocol || view.DefaultBranch != "main" || view.Semantics.Version != 0 {
			t.Fatalf("incorrect frozen metadata or semantics: %+v", metadata)
		}
		got, err := ProjectPendingState(context.Background(), view)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("pending contract changed: %v\ngot %+v\nwant %+v", err, got, want)
		}
		view.Snapshots[0].Message = "edited result"
		_, fresh, err := DecodeRepositoryViewSource(context.Background(), repo, entries)
		if err != nil || fresh.Snapshots[0].Message == "edited result" {
			t.Fatalf("returned input is not owned: %v", err)
		}
	}
}

func TestDecodeRepositoryViewSourceRequiresOriginSupport(t *testing.T) {
	for _, flags := range []map[string]any{{}, {"remote_url_supported": true}, {"remote_url_supported": true, "git_remote_url_supported": false}, {"remote_url_supported": false, "git_remote_url_supported": true}} {
		repo, entries, _ := pendingProjectionFixture(t, 1)
		flags["id"], flags["context_protocol"], flags["default_branch"] = repo, 1, "main"
		for index := range entries {
			if entries[index].Kind == "repository" {
				entries[index].Value = pendingProjectionTestJSON(t, flags)
			}
		}
		if _, _, err := DecodeRepositoryViewSource(context.Background(), repo, entries); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("unsupported origin accepted: %v", err)
		}
		if _, err := ProjectPendingSource(context.Background(), repo, entries); err != nil {
			t.Fatalf("origin-independent pending projection failed: %v", err)
		}
	}
}

func TestDecodeRepositorySourceJSONStrict(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"known":{"value":1,"value":2}}`, `{"known":{"extra":1}}`, `{"known":{}} {}`, `{"unknown":{}}`} {
		var value struct {
			Known struct {
				Value int `json:"value"`
			} `json:"known"`
		}
		if err := DecodeRepositorySourceJSON(context.Background(), []byte(raw), &value); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var value json.RawMessage
	if err := DecodeRepositorySourceJSON(ctx, []byte(`{}`), &value); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
