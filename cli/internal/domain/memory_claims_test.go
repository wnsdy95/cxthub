package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTypedMemoryWirePreservesSourceClaims(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/testdata/memory-claims-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var digest MemoryDigest
	if err := json.Unmarshal(raw, &digest); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, bytes.TrimSpace(raw)) {
		t.Fatalf("memory claims lost during round trip:\n%s", encoded)
	}
}

func TestTypedMemoryValidationAndLegacyIdentity(t *testing.T) {
	valid := func() MemoryDigest {
		return MemoryDigest{ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: HashContent([]byte("source")), Claims: []MemoryClaim{{Kind: "code", Text: "Explicit claim.", Code: &MemoryCodeScope{Commit: strings.Repeat("a", 40), Paths: []string{"src/a.go"}}}}}}}
	}
	for name, mutate := range map[string]func(*MemoryDigest){
		"future version":     func(d *MemoryDigest) { d.ClaimsVersion = 2 },
		"missing version":    func(d *MemoryDigest) { d.ClaimsVersion = 0 },
		"source":             func(d *MemoryDigest) { d.Fragments[0].SourceSnapshot = "invalid" },
		"kind":               func(d *MemoryDigest) { d.Fragments[0].Claims[0].Kind = "guess" },
		"blank":              func(d *MemoryDigest) { d.Fragments[0].Claims[0].Text = " \n" },
		"oversize":           func(d *MemoryDigest) { d.Fragments[0].Claims[0].Text = strings.Repeat("a", 8193) },
		"short sha":          func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Commit = "abc123" },
		"zero sha":           func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Commit = strings.Repeat("0", 40) },
		"parent width":       func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Parent = strings.Repeat("a", 64) },
		"no scope":           func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code = nil },
		"no paths":           func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Paths = nil },
		"path traversal":     func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Paths = []string{"../file"} },
		"absolute":           func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Paths = []string{"/file"} },
		"duplicate":          func(d *MemoryDigest) { d.Fragments[0].Claims[0].Code.Paths = []string{"a", "a"} },
		"rationale code":     func(d *MemoryDigest) { d.Fragments[0].Claims[0].Kind = "rationale" },
		"per fragment bound": func(d *MemoryDigest) { d.Fragments[0].Claims = make([]MemoryClaim, 257) },
	} {
		t.Run(name, func(t *testing.T) {
			d := valid()
			mutate(&d)
			if err := d.ValidateMemoryClaims(); err == nil {
				t.Fatal("invalid claims accepted")
			}
		})
	}
	d := valid()
	if err := d.ValidateMemoryClaims(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"decision", "rationale"} {
		d = valid()
		d.Fragments[0].Claims[0].Kind = kind
		d.Fragments[0].Claims[0].Code = nil
		if err := d.ValidateMemoryClaims(); err != nil {
			t.Fatal(err)
		}
	}
	legacy := MemoryDigest{SnapshotID: HashContent([]byte("legacy")), Summary: "stable", Provider: ProviderCodex}
	raw, _ := json.Marshal(legacy)
	want := `{"snapshot_id":"` + string(legacy.SnapshotID) + `","summary":"stable","key_facts":null,"open_tasks":null,"provider":"codex"}`
	if string(raw) != want {
		t.Fatalf("legacy encoding changed: %s", raw)
	}
}

func TestTypedMemoryMergeRetainsDistinctClaimsAndEmptyVersion(t *testing.T) {
	source := HashContent([]byte("source"))
	a := MemoryDigest{ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: source, Summary: "same", Claims: []MemoryClaim{{Kind: "rationale", Text: "First reason"}}}}}
	b := MemoryDigest{ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: source, Summary: "same", Claims: []MemoryClaim{{Kind: "rationale", Text: "Second reason"}}}}}
	got := MergeDigests(a, b)
	if got.ClaimsVersion != 1 || len(got.Fragments) != 2 {
		t.Fatalf("claims lost: %+v", got)
	}
	got = MergeDigests(got, b)
	if len(got.Fragments) != 2 {
		t.Fatal("idempotent union duplicated claims")
	}
	if MergeDigests(MemoryDigest{ClaimsVersion: 1}, MemoryDigest{}).ClaimsVersion != 1 {
		t.Fatal("empty typed generation downgraded")
	}
	// An old reader drops typed fields; its reconstructed hash must fail identity
	// validation rather than permitting that object to overwrite the attachment.
	raw, _ := json.Marshal(a)
	var old struct {
		SnapshotID ContentHash  `json:"snapshot_id"`
		Summary    string       `json:"summary"`
		KeyFacts   []string     `json:"key_facts"`
		OpenTasks  []string     `json:"open_tasks"`
		Provider   ProviderKind `json:"provider"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	oldRaw, _ := json.Marshal(old)
	if HashContent(oldRaw) == HashContent(raw) {
		t.Fatal("old reader falsely validates a typed object")
	}
}
