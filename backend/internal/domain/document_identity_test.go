package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDocumentIdentityRejectsUnknownAndNull(t *testing.T) {
	for _, raw := range []string{`null`, `3`, `"unknown"`, `"sha256"`} {
		got := DocumentIdentityRootV1
		if err := json.Unmarshal([]byte(raw), &got); !errors.Is(err, ErrUnsupportedDocumentIdentity) || got != DocumentIdentityRootV1 {
			t.Fatalf("identity %s: value=%q err=%v", raw, got, err)
		}
	}
	if _, err := json.Marshal(DocumentIdentity("unknown")); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
		t.Fatal(err)
	}
	for _, identity := range []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1} {
		raw, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		var got DocumentIdentity
		if err := json.Unmarshal(raw, &got); err != nil || got != identity {
			t.Fatal(got, err)
		}
	}
}
