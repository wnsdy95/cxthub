package domain

import (
	"context"
	"strings"
	"testing"
)

func TestLegacySourceHeaderHashPhaseDoesNotRetainRawEnvelope(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, SourceModel: strings.Repeat("m", 2*ConversationManifestChunkBytes+73), Fidelity: FidelityFull}}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	input, output := &legacySourceProofBuffer{body: raw}, &legacySourceProofBuffer{}
	var verifier CanonicalDocVerifier
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), input.input(), output, &verifier)
	if err != nil {
		test.Fatal(err)
	}
	initial, err := preparation.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	maximum := initial
	quanta := 0
	for ; preparation.phase == 0 && quanta < 16; quanta++ {
		before := len(input.reads)
		progress, err := preparation.Step(context.Background())
		retained, retainedErr := preparation.RetainedBytes()
		if err != nil || progress != LegacyByteSourcePending || retainedErr != nil || retained > initial+canonicalCheckBytes ||
			len(input.reads)-before > 2 || len(output.body) != 0 || len(verifier.events) != 0 {
			test.Fatal("source hash pass retained raw envelope payload or changed its original work/publication boundary", quanta, initial, retained, progress, err, retainedErr)
		}
		maximum = max(maximum, retained)
	}
	if preparation.phase != 1 {
		test.Fatal("source header hash pass failed to reach its original decision boundary", preparation.phase)
	}
	test.Logf("source=%dB hash-quanta=%d initial-retained=%dB max-retained=%dB", len(raw), quanta, initial, maximum)
}
