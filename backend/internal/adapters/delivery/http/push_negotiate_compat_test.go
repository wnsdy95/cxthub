package http

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// An older CLI still sends snapshot_haves and reads snapshot_wants. The
// server ignores the first and no longer answers the second, which such a
// client reads as empty.
func TestPushNegotiateIgnoresAnOlderClientsSnapshotHaves(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	doJSON(t, http.MethodPost, ts.URL+"/api/v1/repositories", map[string]any{"name": "OlderNegotiation"}, &repositoryRecord)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{"id": rid, "remote_url": remoteURL, "default_branch": "main"}, nil); code != http.StatusOK {
		t.Fatalf("repo create code %d", code)
	}
	missing := domain.ContentHash("sha256:" + strings.Repeat("e", 64))
	// v0.2.0 sends null on its main upload path and an empty or filled array elsewhere.
	for label, haves := range map[string]any{"null": nil, "a filled array": []domain.ContentHash{missing}} {
		body := map[string]any{"snapshot_haves": haves, "doc_haves": []domain.ContentHash{missing}}
		var answer map[string]any
		if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos/"+url.PathEscape(string(rid))+"/push/negotiate", body, &answer); code != http.StatusOK {
			t.Fatalf("negotiation with snapshot_haves as %s answered %d", label, code)
		}
		if _, present := answer["snapshot_wants"]; present {
			t.Fatalf("negotiation with snapshot_haves as %s still answers snapshot_wants: %+v", label, answer)
		}
		if wants, ok := answer["doc_wants"].([]any); !ok || len(wants) != 1 || wants[0] != string(missing) {
			t.Fatalf("negotiation with snapshot_haves as %s lost the documents it wants: %+v", label, answer)
		}
	}
}
