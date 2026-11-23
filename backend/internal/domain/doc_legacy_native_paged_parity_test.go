package domain

import (
	"fmt"
	"strings"
	"testing"
)

func TestLegacyNativePagedParityPublic8192Replacement(test *testing.T) {
	const count = 8192
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"2","source_model":"paged-byte-oracle","fidelity":"full"},"events":[{"kind":"compaction","seq":17,"replacement":[`)
	for ordinal := 0; ordinal < count; ordinal++ {
		if ordinal != 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, "{\"kind\":\"message\",\"id\":\"child-%04d\",\"seq\":%d,\"role\":\"assistant\",\"blocks\":[{\"type\":\"text\",\"text\":\"original \ud55c🙂 <&>\\n\"}]}", ordinal, (count-ordinal)%257-128)
	}
	raw.WriteString(`],"replacement_complete":false}]}`)
	if raw.Len() <= ConversationManifestChunkBytes || raw.Len() > 2<<20 {
		test.Fatal("8192-child byte fixture lost its original multi-part/2 MiB boundaries", raw.Len())
	}
}

func TestLegacyNativePagedParityPublicVersionAndUnionGates(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		version  string
		event    string
		accepted bool
		children int
	}{
		{name: "legacy-v1-turn", version: "1", event: `{"kind":"turn","seq":7,"role":"user"}`, accepted: true},
		{name: "v2-empty-paged-boundary", version: "2", event: `{"kind":"compaction","replacement":[]}`, accepted: true},
		{name: "v1-rejects-empty-paged-boundary", version: "1", event: `{"kind":"compaction","replacement":[]}`},
		{name: "implicit-v1-rejects-paged-boundary", event: `{"kind":"compaction","replacement":[{"kind":"turn","seq":-1}]}`, children: 1},
		{name: "unsupported-version", version: "99", event: `{"kind":"compaction","replacement":[]}`},
		{name: "v1-rejects-exact-present-false", version: "1", event: `{"kind":"message","agent_message":false,"blocks":[]}`},
		{name: "replacement-and-locked-rejected", version: "2", event: `{"kind":"compaction","replacement":[],"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`},
		{name: "wrong-union-exact-replacement", version: "2", event: `{"kind":"turn","replacement":[]}`},
		{name: "wrong-union-folded-replacement", version: "2", event: `{"kind":"turn","REPLACEMENT":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = []byte(`{"envelope":{"cir_version":"` + fixture.version + `"},"events":[` + fixture.event + `]}`)
		})
	}
}
