package domain

import (
	"testing"
)

func TestStreamedLegacyDocByteRangeGenerationRejectsInvalidProof(test *testing.T) {
	_, raw := legacyDocRangeGenerationFixture(test, 1)
	valid, spool := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
	_ = valid.Manifest()
	spool.readTransform = func(body []byte) []byte {
		test.Fatal("invalid legacy byte proof read source bytes")
		return body
	}
	for name, invalidate := range map[string]func(*VerifiedLegacyStream){
		"zero":              func(proof *VerifiedLegacyStream) { *proof = VerifiedLegacyStream{} },
		"missing-source":    func(proof *VerifiedLegacyStream) { proof.source = nil },
		"missing-reference": func(proof *VerifiedLegacyStream) { proof.reference = VerifiedDocReference{} },
		"wrong-format":      func(proof *VerifiedLegacyStream) { proof.manifest.Format = ChunkFormatV1 },
		"root-identity":     func(proof *VerifiedLegacyStream) { proof.reference.identity = DocumentIdentityRootV1 },
		"unknown-identity":  func(proof *VerifiedLegacyStream) { proof.reference.identity = "future" },
	} {
		test.Run(name, func(test *testing.T) {
			proof := valid
			invalidate(&proof)
		})
	}
}
