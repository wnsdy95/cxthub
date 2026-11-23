package domain

import (
	"strings"
	"testing"
)

func legacyNativeOutputReviewString(test *testing.T, prefix string) string {
	test.Helper()
	const payloadBytes = 5*ConversationManifestChunkBytes + 73
	var scalar strings.Builder
	for _, boundary := range []struct {
		offset int
		token  string
		split  int
	}{
		{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
		{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
		{offset: 3 * ConversationManifestChunkBytes, token: `\uD800x`, split: 4},
		{offset: 4 * ConversationManifestChunkBytes, token: "\xf0\x9f", split: 1},
		{offset: 5 * ConversationManifestChunkBytes, token: `\uD800\uD801\uDC00`, split: 8},
	} {
		padding := boundary.offset - len(prefix) - 1 - scalar.Len() - boundary.split
		if padding < 0 {
			test.Fatal("output boundary fixture overlaps an earlier token", boundary.offset)
		}
		scalar.WriteString(strings.Repeat("x", padding))
		scalar.WriteString(boundary.token)
	}
	scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uDC00`)
	scalar.WriteString("\ud55c<&>\xff\xc0\x80")
	const finalSurrogate = `\uD800`
	padding := payloadBytes - scalar.Len() - len(finalSurrogate)
	if padding < 0 {
		test.Fatal("output boundary fixture exceeded its five-part payload", scalar.Len())
	}
	scalar.WriteString(strings.Repeat("x", padding))
	scalar.WriteString(finalSurrogate)
	return `"` + scalar.String() + `"`
}
