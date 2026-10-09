package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFrozenCaptureInputValidation(t *testing.T) {
	valid := FrozenCaptureInput{Version: 1, Provider: ProviderClaude, SourcePath: "/synthetic/native.jsonl", Size: 1,
		Hash: HashContent([]byte("x")), Chunks: []FrozenCaptureChunk{{Hash: HashContent([]byte("x")), Size: 1}},
		CapturedAt: time.Now().UTC(), PolicyFingerprint: strings.Repeat("a", 64)}
	for name, change := range map[string]func(*FrozenCaptureInput){
		"version":       func(in *FrozenCaptureInput) { in.Version++ },
		"provider":      func(in *FrozenCaptureInput) { in.Provider = "other" },
		"relative":      func(in *FrozenCaptureInput) { in.SourcePath = "native.jsonl" },
		"traversal":     func(in *FrozenCaptureInput) { in.SourcePath = "/tmp/../native.jsonl" },
		"empty":         func(in *FrozenCaptureInput) { in.Size = 0 },
		"large":         func(in *FrozenCaptureInput) { in.Size = FrozenCaptureMaxBytes + 1 },
		"hash":          func(in *FrozenCaptureInput) { in.Hash = "bad" },
		"chunk-hash":    func(in *FrozenCaptureInput) { in.Chunks[0].Hash = "../escape" },
		"chunk-size":    func(in *FrozenCaptureInput) { in.Chunks[0].Size++ },
		"missing-chunk": func(in *FrozenCaptureInput) { in.Chunks = nil },
		"time":          func(in *FrozenCaptureInput) { in.CapturedAt = time.Time{} },
		"policy":        func(in *FrozenCaptureInput) { in.PolicyFingerprint = strings.Repeat("G", 64) },
		"identity":      func(in *FrozenCaptureInput) { in.DocIdentity = "future" },
	} {
		t.Run(name, func(t *testing.T) {
			in := valid
			in.Chunks = append([]FrozenCaptureChunk(nil), valid.Chunks...)
			change(&in)
			if in.Validate() == nil {
				t.Fatal("accepted malformed frozen input")
			}
		})
	}
	for _, identity := range []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1} {
		valid.DocIdentity = identity
		b, err := json.Marshal(valid)
		if err != nil {
			t.Fatal(err)
		}
		var decoded FrozenCaptureInput
		if err := json.Unmarshal(b, &decoded); err != nil || decoded.Validate() != nil || decoded.DocIdentity != identity || decoded.SourcePath != valid.SourcePath || decoded.Size != valid.Size {
			t.Fatal("descriptor round trip", err)
		}
	}
	valid.Size = FrozenCaptureMaxBytes
	valid.Chunks = make([]FrozenCaptureChunk, FrozenCaptureMaxBytes/FrozenCaptureChunkBytes)
	for i := range valid.Chunks {
		valid.Chunks[i] = FrozenCaptureChunk{Hash: valid.Hash, Size: FrozenCaptureChunkBytes}
	}
	if err := valid.Validate(); err != nil {
		t.Fatal("exact hard bound", err)
	}
}
