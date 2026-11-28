package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIdentityDeclarationCannotBeErasedDuringDecode(t *testing.T) {
	field := "identity"
	for _, alias := range []string{field, strings.ToUpper(field), field[:1] + strings.ToUpper(field[1:]), fmt.Sprintf(`\u%04x%s`, field[0], field[1:])} {
		for _, values := range [][2]string{{ConversationManifestIdentity, ""}, {"", ConversationManifestIdentity}, {"", ""}} {
			raw := fmt.Sprintf(`{"%s":%q,"%s":%q}`, field, values[0], alias, values[1])
			before := SessionDoc{Hash: "original", Identity: DocumentIdentityRootV1}
			got := before
			if err := json.Unmarshal([]byte(raw), &got); err == nil || !reflect.DeepEqual(got, before) {
				t.Fatalf("document downgrade %s: %+v %v", raw, got, err)
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
}
