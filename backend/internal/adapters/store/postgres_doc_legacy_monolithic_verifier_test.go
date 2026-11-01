//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyMonolithicVerifierCase struct {
	name     string
	raw      []byte
	hash     domain.ContentHash
	expected domain.VerifiedSessionDoc
	wantErr  error
	canceled bool
}

func legacyMonolithicVerifierProof(test testing.TB, document domain.CIRDocument) domain.VerifiedSessionDoc {
	test.Helper()
	canonical, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: document})
	if err != nil || !proof.Valid() || !bytes.Equal(proof.Bytes(), canonical) {
		test.Fatal("invalid typed canonical fixture", err)
	}
	return proof
}

func legacyMonolithicVerifierCases(test testing.TB) []legacyMonolithicVerifierCase {
	test.Helper()
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SourceModel: "synthetic", SessionOriginID: "monolithic-verifier", Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "original <\ud55c🙂> \"\\\n"}}},
			{Kind: domain.EventToolCall, Seq: 4, CallID: "call", ToolName: "synthetic",
				Input: map[string]any{"count": float64(7), "label": "original-tool"}},
			{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "original answer"}}},
		},
	}
	expected := legacyMonolithicVerifierProof(test, document)
	canonical := expected.Bytes()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
		test.Fatal(err)
	}
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &parts); err != nil {
		test.Fatal(err)
	}
	reorderedKeys, err := json.Marshal(struct {
		Events   []json.RawMessage `json:"events"`
		Envelope json.RawMessage   `json:"envelope"`
	}{Events: parts.Events, Envelope: parts.Envelope})
	if err != nil {
		test.Fatal(err)
	}
	reversedStream := bytes.Join([][]byte{parts.Events[2], parts.Events[1], parts.Events[0]}, []byte(","))
	reversedEvents := domain.AssembleCanonicalDoc(parts.Envelope, [][]byte{reversedStream})
	changedDocument := document
	changedDocument.Envelope.SourceModel = "changed-model"
	changed := legacyMonolithicVerifierProof(test, changedDocument)
	duplicateDocument := document
	duplicateDocument.Events = []domain.CIREvent{document.Events[0], document.Events[0], document.Events[1], document.Events[2]}
	duplicates := legacyMonolithicVerifierProof(test, duplicateDocument)
	versionOneDocument := document
	versionOneDocument.Envelope.CIRVersion = "1"
	versionOne := legacyMonolithicVerifierProof(test, versionOneDocument)
	unsupported := bytes.Replace(canonical, []byte(`"cir_version":"2"`), []byte(`"cir_version":"99"`), 1)
	invalidUnion := bytes.Replace(canonical, []byte(`"kind":"tool_call"`), []byte(`"kind":"message"`), 1)
	invalidDuplicate := bytes.Replace(canonical, []byte(`"seq":2`), []byte(`"seq":"invalid","seq":2`), 1)
	versionOnePresence := bytes.Replace(versionOne.Bytes(), []byte(`"kind":"message"`),
		[]byte(`"agent_message":false,"agent_message":null,"kind":"message"`), 1)
	malformed := canonical[:len(canonical)-1]
	cases := []legacyMonolithicVerifierCase{
		{name: "canonical", raw: canonical, hash: expected.Hash(), expected: expected},
		{name: "canonical-v1", raw: versionOne.Bytes(), hash: versionOne.Hash(), expected: versionOne},
		{name: "pretty-json-normalized-hash", raw: pretty.Bytes(), hash: expected.Hash(), expected: expected},
		{name: "noncanonical-key-order-normalized-hash", raw: reorderedKeys, hash: expected.Hash(), expected: expected},
		{name: "out-of-order-events-normalized-hash", raw: reversedEvents, hash: expected.Hash(), expected: expected},
		{name: "duplicate-events-preserved", raw: duplicates.Bytes(), hash: duplicates.Hash(), expected: duplicates},
		{name: "changed-canonical-semantics-old-hash", raw: changed.Bytes(), hash: expected.Hash(), wantErr: domain.ErrIntegrity},
		{name: "changed-canonical-semantics-new-hash", raw: changed.Bytes(), hash: changed.Hash(), expected: changed},
		{name: "wrong-claimed-hash", raw: canonical, hash: domain.HashContent([]byte("wrong hash")), wantErr: domain.ErrIntegrity},
		{name: "pretty-json-raw-hash", raw: pretty.Bytes(), hash: domain.HashContent(pretty.Bytes()), wantErr: domain.ErrIntegrity},
		{name: "noncanonical-key-order-raw-hash", raw: reorderedKeys, hash: domain.HashContent(reorderedKeys), wantErr: domain.ErrIntegrity},
		{name: "out-of-order-events-raw-hash", raw: reversedEvents, hash: domain.HashContent(reversedEvents), wantErr: domain.ErrIntegrity},
		{name: "malformed-json-normalized-hash", raw: malformed, hash: expected.Hash(), wantErr: domain.ErrIntegrity},
		{name: "malformed-json-raw-hash", raw: malformed, hash: domain.HashContent(malformed), wantErr: domain.ErrIntegrity},
		{name: "invalid-event-union-normalized-hash", raw: invalidUnion, hash: expected.Hash(), wantErr: domain.ErrIntegrity},
		{name: "invalid-event-union-raw-hash", raw: invalidUnion, hash: domain.HashContent(invalidUnion), wantErr: domain.ErrIntegrity},
		{name: "invalid-duplicate-type-normalized-hash", raw: invalidDuplicate, hash: expected.Hash(), wantErr: domain.ErrIntegrity},
		{name: "invalid-duplicate-type-raw-hash", raw: invalidDuplicate, hash: domain.HashContent(invalidDuplicate), wantErr: domain.ErrIntegrity},
		{name: "v1-duplicate-v2-field-presence", raw: versionOnePresence, hash: versionOne.Hash(), wantErr: domain.ErrIntegrity},
		{name: "unsupported-version-normalized-hash", raw: unsupported, hash: expected.Hash(), wantErr: domain.ErrIntegrity},
		{name: "unsupported-version-raw-hash-no-fallback", raw: unsupported, hash: domain.HashContent(unsupported), wantErr: domain.ErrUnsupportedCIRVersion},
		{name: "canceled-canonical", raw: canonical, hash: expected.Hash(), wantErr: context.Canceled, canceled: true},
		{name: "canceled-normalization", raw: pretty.Bytes(), hash: expected.Hash(), wantErr: context.Canceled, canceled: true},
		{name: "canceled-malformed-exact-hash", raw: malformed, hash: domain.HashContent(malformed), wantErr: context.Canceled, canceled: true},
		{name: "canceled-unsupported-exact-hash", raw: unsupported, hash: domain.HashContent(unsupported), wantErr: context.Canceled, canceled: true},
	}
	for _, variant := range []struct {
		name string
		raw  []byte
	}{
		{name: "unknown-document-field", raw: bytes.Replace(canonical, []byte(`{"envelope":`), []byte(`{"unknown":true,"envelope":`), 1)},
		{name: "unknown-envelope-field", raw: bytes.Replace(canonical, []byte(`"cir_version":"2"`), []byte(`"cir_version":"2","unknown":true`), 1)},
		{name: "unknown-event-field", raw: bytes.Replace(canonical, []byte(`"kind":"message"`), []byte(`"kind":"message","unknown":true`), 1)},
		{name: "duplicate-document-key", raw: bytes.Replace(canonical, []byte(`{"envelope":`), []byte(`{"events":[],"envelope":`), 1)},
		{name: "duplicate-envelope-key", raw: bytes.Replace(canonical, []byte(`"cir_version":"2"`), []byte(`"cir_version":"1","cir_version":"2"`), 1)},
		{name: "duplicate-event-key", raw: bytes.Replace(canonical, []byte(`"seq":2`), []byte(`"seq":0,"seq":2`), 1)},
		{name: "duplicate-tool-input-key", raw: bytes.Replace(canonical, []byte(`"count":7`), []byte(`"count":0,"count":7`), 1)},
	} {
		if bytes.Equal(variant.raw, canonical) || domain.HashContent(variant.raw) == expected.Hash() {
			test.Fatalf("%s fixture must require normalization", variant.name)
		}
		cases = append(cases,
			legacyMonolithicVerifierCase{name: variant.name + "/normalized-hash", raw: variant.raw, hash: expected.Hash(), expected: expected},
			legacyMonolithicVerifierCase{name: variant.name + "/raw-hash", raw: variant.raw, hash: domain.HashContent(variant.raw), wantErr: domain.ErrIntegrity},
		)
	}
	return cases
}

func checkLegacyMonolithicVerifierCase(test *testing.T, storage *PostgresStore, testCase legacyMonolithicVerifierCase) {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if testCase.canceled {
		cancel()
	}
	raw := bytes.Clone(testCase.raw)
	legacy, legacyErr := verifyLegacyDecodedMonolithicDoc(ctx, testCase.hash, raw)
	if (legacyErr == nil) != (testCase.wantErr == nil) {
		test.Fatalf("legacy acceptance differs from pinned expectation: %v", legacyErr)
	}
	if legacyErr != nil {
		legacyWantErr := domain.ErrIntegrity
		if testCase.canceled {
			legacyWantErr = context.Canceled
		}
		if !errors.Is(legacyErr, legacyWantErr) || legacy.Valid() {
			test.Fatalf("legacy failure returned valid=%t, error=%v; want %v", legacy.Valid(), legacyErr, legacyWantErr)
		}
	} else if !legacy.Valid() || legacy.DocumentRef() != testCase.expected.DocumentRef() ||
		!bytes.Equal(legacy.Bytes(), testCase.expected.Bytes()) {
		test.Fatal("legacy proof differs from the typed canonical expectation")
	}
	checkLegacyNormalizationDecoderParity(test, ctx, testCase.hash, raw, legacy, legacyErr)
	proof, err := storage.verifyLegacyMonolithicRangeSource(ctx, testCase.hash, raw)
	if (err == nil) != (legacyErr == nil) || !errors.Is(err, testCase.wantErr) {
		test.Fatalf("verification acceptance/error differs: got %v, legacy %v, want %v", err, legacyErr, testCase.wantErr)
	}
	if !bytes.Equal(raw, testCase.raw) {
		test.Fatal("verification modified caller-owned source bytes")
	}
	if err != nil {
		if proof.Valid() || proof.Reference().Valid() {
			test.Fatal("failed verification returned a valid proof")
		}
		return
	}
	if !proof.Valid() || !proof.Reference().Valid() || proof.Hash() != legacy.Hash() ||
		proof.DocumentRef() != legacy.DocumentRef() || proof.Reference() != legacy.Reference() ||
		!bytes.Equal(proof.Bytes(), legacy.Bytes()) {
		test.Fatal("verified canonical bytes or reference differ from legacy normalization")
	}
	for position := range raw {
		raw[position] = '!'
	}
	exported := proof.Bytes()
	exported[0] = '?'
	if !proof.Valid() || proof.Reference() != legacy.Reference() || proof.DocumentRef() != legacy.DocumentRef() ||
		!bytes.Equal(proof.Bytes(), legacy.Bytes()) {
		test.Fatal("caller source or exported-byte mutation changed the proof")
	}
	if changedProof, changedErr := storage.verifyLegacyMonolithicRangeSource(ctx, testCase.hash, raw); !errors.Is(changedErr, domain.ErrIntegrity) || changedProof.Valid() {
		test.Fatal("previous verification accepted mutated current source bytes", changedErr)
	}
}

func TestPostgresLegacyMonolithicRangeSourceParity(test *testing.T) {
	cases := legacyMonolithicVerifierCases(test)
	var storage PostgresStore
	test.Run("repeated", func(test *testing.T) {
		for _, testCase := range cases {
			test.Run(testCase.name, func(test *testing.T) {
				for range 3 {
					checkLegacyMonolithicVerifierCase(test, &storage, testCase)
				}
			})
		}
	})
	test.Run("concurrent", func(test *testing.T) {
		for worker := range 8 {
			test.Run(fmt.Sprintf("worker-%d", worker), func(test *testing.T) {
				test.Parallel()
				for _, testCase := range cases {
					test.Run(testCase.name, func(test *testing.T) {
						checkLegacyMonolithicVerifierCase(test, &storage, testCase)
					})
				}
			})
		}
	})
}

func FuzzLegacyMonolithicRangeSourceParity(fuzzer *testing.F) {
	for _, testCase := range legacyMonolithicVerifierCases(fuzzer) {
		fuzzer.Add(testCase.raw)
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			test.Skip()
		}
		hashes := []domain.ContentHash{domain.HashContent(raw)}
		var document domain.CIRDocument
		if json.Unmarshal(raw, &document) == nil {
			if canonical, err := domain.CanonicalBytes(document); err == nil {
				hashes = append(hashes, domain.HashContent(canonical))
			}
		}
		var storage PostgresStore
		for _, hash := range hashes {
			legacy, legacyErr := verifyLegacyDecodedMonolithicDoc(context.Background(), hash, raw)
			for range 2 {
				proof, err := storage.verifyLegacyMonolithicRangeSource(context.Background(), hash, raw)
				if (err == nil) != (legacyErr == nil) {
					test.Fatal("canonical/fallback acceptance differs", err, legacyErr)
				}
				if err == nil && (proof.DocumentRef() != legacy.DocumentRef() || !bytes.Equal(proof.Bytes(), legacy.Bytes())) {
					test.Fatal("accepted source changed its canonical identity or bytes")
				}
				if err != nil && proof.Valid() {
					test.Fatal("rejected source returned a valid proof", err)
				}
			}
		}
	})
}
