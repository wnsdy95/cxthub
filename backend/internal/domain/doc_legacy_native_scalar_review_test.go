package domain

import (
	"strings"
	"testing"
)

func TestLegacyNativeScalarReviewGiantStringsMatchTypedOracle(test *testing.T) {
	for _, field := range []struct {
		name string
		key  string
	}{
		{name: "redacted_summary", key: `"redacted_\u0073ummary"`},
		{name: "id", key: `"ID"`},
		{name: "ts", key: `"tſ"`},
	} {
		test.Run(field.name, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"reasoning","seq":7,` + field.key + `:"`
			var scalar strings.Builder
			for _, boundary := range []struct {
				offset int
				token  string
				split  int
			}{
				{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
				{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
			} {
				padding := boundary.offset - len(prefix) - scalar.Len() - boundary.split
				if padding < 0 {
					test.Fatal("scalar boundary fixture overlaps an earlier token", boundary.offset)
				}
				scalar.WriteString(strings.Repeat("x", padding))
				scalar.WriteString(boundary.token)
			}
			scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uD800x\uDC00\uD800\uD801\uDC00`)
			scalar.WriteString("\ud55c<&>\xff\xc0\x80\xf0\x9f")
			scalar.WriteString(`\uD800`)
			_ = []byte(prefix + scalar.String() + `"},{"kind":"turn","seq":1,"id":"first"},{"kind":"turn","seq":7,"id":"tie-last"}]}`)
		})
	}
}
