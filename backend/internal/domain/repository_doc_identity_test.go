package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestP6RepositoryIdentityWire(t *testing.T) {
	var r Repo
	if err := json.Unmarshal([]byte(`{"required_doc_identity":"cxt-manifest-sha256-v1"}`), &r); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"required_doc_identity":"cxt-manifest-sha256-v1"`) {
		t.Fatal("requirement lost")
	}
	raw, _ = json.Marshal(Repo{})
	if strings.Contains(string(raw), "required_doc_identity") {
		t.Fatal("legacy omission changed")
	}
	for _, raw := range []string{`{"required_doc_identity":null}`, `{"required_doc_identity":"unknown"}`} {
		if json.Unmarshal([]byte(raw), &r) == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}
