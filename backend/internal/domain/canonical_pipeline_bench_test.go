package domain

import (
	"strings"
	"testing"
)

func canonicalPipelineFixture(b *testing.B) SessionDoc {
	b.Helper()
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceProvider: ProviderCodex, Fidelity: FidelityFull}}
	for i := 0; i < 256; i++ {
		cir.Events = append(cir.Events, CIREvent{Seq: i, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("synthetic text ", 1024)}}})
	}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	return SessionDoc{Hash: HashContent(raw), CIR: cir}
}
func BenchmarkLegacyCanonicalPipeline(b *testing.B) {
	d := canonicalPipelineFixture(b)
	b.ResetTimer()
	for b.Loop() {
		if err := ValidateSessionDocHash(d); err != nil {
			b.Fatal(err)
		}
		if err := ValidateSessionDocHash(d); err != nil {
			b.Fatal(err)
		}
		if _, err := ValidatedSessionDocBytes(d); err != nil {
			b.Fatal(err)
		}
		if _, err := BuildDocReadIndex(d); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkVerifiedCanonicalPipeline(b *testing.B) {
	d := canonicalPipelineFixture(b)
	b.ResetTimer()
	for b.Loop() {
		v, err := VerifySessionDoc(d)
		if err != nil {
			b.Fatal(err)
		}
		if HashContent(v.Bytes()) != d.Hash {
			b.Fatal("bytes changed")
		}
		if _, err := v.ReadIndex(); err != nil {
			b.Fatal(err)
		}
	}
}
