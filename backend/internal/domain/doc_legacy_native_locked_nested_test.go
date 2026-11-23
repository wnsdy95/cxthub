package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func legacyNativeLockedNestedEvents() []struct {
	name   string
	fields string
} {
	return []struct {
		name   string
		fields string
	}{
		{name: "reasoning", fields: `"kind":"reasoning","cross_replayable":false,"redacted_summary":"visible reasoning"`},
		{name: "agent-message", fields: `"kind":"message","role":"assistant","blocks":[{"type":"text","text":"visible agent message"}],"agent_message":true,"agent_author":"worker","agent_recipient":"parent"`},
		{name: "compaction", fields: `"kind":"compaction"`},
	}
}

func legacyNativeLockedNestedString(test *testing.T, prefix string) string {
	test.Helper()
	var scalar strings.Builder
	for _, boundary := range []struct {
		offset int
		token  string
		split  int
	}{
		{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
		{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
		{offset: 3 * ConversationManifestChunkBytes, token: `\uD800x`, split: 4},
		{offset: 4 * ConversationManifestChunkBytes, token: `\"`, split: 1},
		{offset: 5 * ConversationManifestChunkBytes, token: `\uD800\uD801\uDC00`, split: 8},
	} {
		padding := boundary.offset - len(prefix) - 1 - scalar.Len() - boundary.split
		if padding < 0 {
			test.Fatal("locked scalar boundary overlaps an earlier token", boundary.offset)
		}
		scalar.WriteString(strings.Repeat("x", padding))
		scalar.WriteString(boundary.token)
	}
	scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uDC00`)
	scalar.WriteString("\ud55c<&>")
	const ending = `\uD800`
	padding := 5*ConversationManifestChunkBytes + 73 - scalar.Len() - len(ending)
	if padding < 0 {
		test.Fatal("locked scalar exceeded its fixed giant payload", scalar.Len())
	}
	scalar.WriteString(strings.Repeat("x", padding))
	scalar.WriteString(ending)
	return `"` + scalar.String() + `"`
}

func TestLegacyNativeLockedNestedBlockViewsMatchTypedOracle(test *testing.T) {
	paged := `[{"type":"text","text":"first"},` + strings.Repeat(`{"type":"text","text":"middle"},`, 7) + `{"type":"text","text":"last"}]`
	for _, fixture := range []struct {
		name   string
		fields string
	}{
		{name: "missing"},
		{name: "null", fields: `,"blocks":null`},
		{name: "empty", fields: `,"blocks":[]`},
		{name: "single", fields: ",\"blocks\":[{\"type\":\"text\",\"text\":\"visible \ud55c🙂<&>\\n\"}]"},
		{name: "inline", fields: `,"blocks":[{"type":"text","text":"first"},{"type":"text","text":"last"}]`},
		{name: "paged", fields: `,"blocks":` + paged},
		{name: "shortened-merge", fields: `,"blocks":` + paged + `,"BLOCKS":[{"text":"merged"}]`},
		{name: "empty-reset", fields: `,"blocks":` + paged + `,"blocks":[]`},
		{name: "null-reset", fields: `,"blocks":` + paged + `,"blocks":null`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = `{"id":"locked","seq":7,"kind":"message","role":"assistant","agent_message":true,` +
				`"agent_author":"worker","agent_recipient":"parent","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}` + fixture.fields + `}`
		})
	}
}

func TestLegacyNativeLockedNestedGiantStringsMatchTypedOracle(test *testing.T) {
	for _, shape := range legacyNativeLockedNestedEvents() {
		for _, field := range []string{"blob", "scheme"} {
			for _, keys := range []struct {
				name   string
				locked string
				blob   string
				scheme string
			}{
				{name: "exact", locked: `"locked"`, blob: `"blob"`, scheme: `"scheme"`},
				{name: "escaped", locked: `"loc\u006bed"`, blob: `"blo\u0062"`, scheme: `"\u0073cheme"`},
				{name: "casefold", locked: `"LOCKED"`, blob: `"BLOB"`, scheme: `"ſCHEME"`},
			} {
				test.Run(shape.name+"/"+field+"/"+keys.name, func(test *testing.T) {
					prefix := `{"envelope":{"cir_version":"2"},"events":[{"id":"locked","seq":7,` + shape.fields + `,` + keys.locked + `:{"provider":"codex",`
					selected, other := keys.blob, keys.scheme+`:"encrypted_content",`
					if field == "scheme" {
						selected, other = keys.scheme, keys.blob+`:"opaque",`
					}
					prefix += other + selected + `:`
					giant := legacyNativeLockedNestedString(test, prefix)
					var decoded string
					if err := json.Unmarshal([]byte(giant), &decoded); err != nil || len(giant)-2 != 5*ConversationManifestChunkBytes+73 {
						test.Fatal("giant locked string lost its native acceptance or exact size", err, len(giant)-2)
					}
					expected := LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}
					if field == "blob" {
						expected.Blob = decoded
					} else {
						expected.Scheme = decoded
					}
					_ = strings.TrimPrefix(prefix, `{"envelope":{"cir_version":"2"},"events":[`) + giant + `},"ts":"2026-10-25T00:00:00Z"}`
				})
			}
		}
	}
}

func TestLegacyNativeLockedNestedDuplicateMergeAndNullMatchTypedOracle(test *testing.T) {
	blob := strings.Repeat("b", 2*ConversationManifestChunkBytes+73) + "\ud55c🙂<&>\n"
	scheme := strings.Repeat("s", 2*ConversationManifestChunkBytes+91) + "\u2028\u2029\"\\"
	blobJSON, err := json.Marshal(blob)
	if err != nil {
		test.Fatal(err)
	}
	schemeJSON, err := json.Marshal(scheme)
	if err != nil {
		test.Fatal(err)
	}
	for _, shape := range legacyNativeLockedNestedEvents() {
		for _, fixture := range []struct {
			name     string
			fields   string
			expected LockedBlob
		}{
			{name: "duplicate-object-keeps-giant-blob", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `},"LOCKED":{"scheme":"signature"}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "signature", Blob: blob}},
			{name: "duplicate-object-keeps-giant-scheme", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":"old"},"locKed":{"blob":"fresh"}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: "fresh"}},
			{name: "scalar-nulls-and-empty-object-retain-giants", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":` + string(blobJSON) + `},"loc\u006bed":{"provider":null,"ſcheme":null,"BLOB":null},"LOCKED":{}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: blob}},
			{name: "short-blob-overwrite-then-null", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `,"BLOB":"fresh\nvalue","\u0062lob":null}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "fresh\nvalue"}},
			{name: "short-scheme-overwrite-then-null", fields: `"locked":{"provider":"codex","blob":"opaque","scheme":` + string(schemeJSON) + `,"ſcheme":"signature","SCHEME":null}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "signature", Blob: "opaque"}},
			{name: "escaped-giants-merge-into-existing-object", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":"old"},"LOCKED":{"blo\u0062":` + string(blobJSON) + `},"locKed":{"\u0073cheme":` + string(schemeJSON) + `}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: blob}},
			{name: "outer-null-resets-giant-blob-before-new-object", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `},"locKed":null,"loc\u006bed":{"provider":"claude","scheme":"signature","blob":null}`, expected: LockedBlob{Provider: ProviderClaude, Scheme: "signature"}},
			{name: "outer-null-resets-giant-scheme-before-new-object", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":"stale"},"LOCKED":null,"locked":{"provider":"claude","scheme":"signature","blob":null}`, expected: LockedBlob{Provider: ProviderClaude, Scheme: "signature"}},
		} {
			test.Run(shape.name+"/"+fixture.name, func(test *testing.T) {
				_ = `{"id":"locked","seq":7,` + shape.fields + `,` + fixture.fields + `,"ts":"2026-10-25T00:00:00Z"}`
			})
		}
	}
}
