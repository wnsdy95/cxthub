package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIdentityDeclarationCannotBeErasedDuringDecode(t *testing.T) {
	for _, field := range []string{"identity", "doc_identity"} {
		for _, alias := range []string{field, strings.ToUpper(field), field[:1] + strings.ToUpper(field[1:]), fmt.Sprintf(`\u%04x%s`, field[0], field[1:])} {
			for _, values := range [][2]string{{ConversationManifestIdentity, ""}, {"", ConversationManifestIdentity}, {"", ""}} {
				raw := fmt.Sprintf(`{"%s":%q,"%s":%q}`, field, values[0], alias, values[1])
				if field == "identity" {
					before := SessionDoc{Hash: "original", Identity: DocumentIdentityRootV1}
					got := before
					if err := json.Unmarshal([]byte(raw), &got); err == nil || !reflect.DeepEqual(got, before) {
						t.Fatalf("document downgrade %s: %+v %v", raw, got, err)
					}
				} else {
					before := Snapshot{ID: "original", DocIdentity: DocumentIdentityRootV1}
					got := before
					if err := json.Unmarshal([]byte(raw), &got); err == nil || !reflect.DeepEqual(got, before) {
						t.Fatalf("snapshot downgrade %s: %+v %v", raw, got, err)
					}
				}
			}
		}
	}
}

func TestIdentityDecodeDoesNotConfuseDestinationReuseWithDuplicateKeys(t *testing.T) {
	doc := SessionDoc{Hash: "preserved", Identity: DocumentIdentityRootV1}
	for _, raw := range []string{`{"identity":""}`, `{"identity":"cxt-manifest-sha256-v1"}`, `null`, `{}`} {
		if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc.Hash != "preserved" {
			t.Fatal(raw, doc, err)
		}
	}
	snap := Snapshot{ID: "preserved", DocIdentity: DocumentIdentityRootV1}
	for _, raw := range []string{`{"doc_identity":""}`, `{"doc_identity":"cxt-manifest-sha256-v1"}`, `null`, `{}`} {
		if err := json.Unmarshal([]byte(raw), &snap); err != nil || snap.ID != "preserved" {
			t.Fatal(raw, snap, err)
		}
	}
}
