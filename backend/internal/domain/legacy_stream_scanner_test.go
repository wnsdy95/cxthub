package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLegacyStreamSingleEventResourceLimit(t *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("Mixed case ", 100)}}}}}
	raw, err := CanonicalBytes(document)
	if err != nil {
		t.Fatal(err)
	}
	envelope, event := chunkVerifierParts(t, raw)
	for _, warm := range []bool{false, true} {
		for _, limit := range []int{0, 1, 1024, len(event) - 1, len(event), len(event) + 1} {
			t.Run(fmt.Sprintf("warm=%t/limit=%d", warm, limit), func(t *testing.T) {
				var verifier CanonicalDocVerifier
				if warm {
					if _, err := verifier.Verify(context.Background(), HashContent(raw), raw); err != nil {
						t.Fatal(err)
					}
				}
				input := chunkVerifierPlan(envelope, event, 7)
				proof, err := verifier.verifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{}, limit, MaxFinalizedDocBytes)
				if (err == nil) != (limit >= len(event)) {
					t.Fatalf("event limit was not enforced: %v", err)
				}
				if err != nil && (!errors.Is(err, ErrDocumentResourceLimit) || errors.Is(err, ErrIntegrity) || proof.Valid()) {
					t.Fatalf("valid oversized input mislabeled: %v", err)
				}
				scanner := legacyStreamScanner{limit: limit, visit: func(canonicalSpan, []byte) (int, error) { return 0, nil }}
				err = scanner.add(context.Background(), event)
				if cap(scanner.event) > limit || len(scanner.event) > limit || (err == nil) != (limit >= len(event)) {
					t.Fatalf("scratch grew past its limit: len=%d cap=%d err=%v", len(scanner.event), cap(scanner.event), err)
				}
			})
		}
	}
	input := buildDocChunkPlan(ChunkFormatV2, envelope, [][]byte{event, []byte(","), event})
	var verifier CanonicalDocVerifier
	canonical := AssembleCanonicalDoc(envelope, [][]byte{event, event})
	if proof, err := verifier.verifyLegacyStream(context.Background(), HashContent(canonical), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{}, len(event), MaxFinalizedDocBytes); err != nil || !proof.Valid() {
		t.Fatal("event limit incorrectly applied to aggregate stream", err)
	}
	if MaxLegacyStreamEventBytes != 32<<20 {
		t.Fatal("worker event memory contract changed")
	}
}

func TestLegacyStreamDepthBoundaryParity(t *testing.T) {
	envelope, err := canonicalJSON(CIREnvelope{CIRVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	event, err := canonicalJSON(CIREvent{Kind: EventToolCall, Input: map[string]any{"x": 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"array", "object"} {
		for nesting := 9995; nesting <= 9998; nesting++ {
			t.Run(fmt.Sprintf("%s/%d", shape, nesting), func(t *testing.T) {
				open, close := "[", "]"
				if shape == "object" {
					open, close = `{"x":`, "}"
				}
				body := bytes.Replace(event, []byte(`"x":0`), []byte(`"x":`+strings.Repeat(open, nesting)+"0"+strings.Repeat(close, nesting)), 1)
				raw := AssembleCanonicalDoc(envelope, [][]byte{body})
				input := chunkVerifierPlan(envelope, body, 4093)
				for _, warm := range []bool{false, true} {
					var verifier CanonicalDocVerifier
					if warm {
						var pending []canonicalEventProof
						if _, err := verifier.event(context.Background(), "2", body, &pending); err != nil {
							t.Fatal(err)
						}
						verifier.remember(pending)
					}
					_, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
					oracle := canonicalVerificationOracle(raw)
					if (err == nil) != (oracle == nil) {
						t.Fatalf("warm=%t stream=%v oracle=%v", warm, err, oracle)
					}
				}
			})
		}
	}
}

func FuzzLegacyStreamOracleParity(f *testing.F) {
	raw := verifierFixture(f)
	envelope, events := chunkVerifierParts(f, raw)
	f.Add(string(envelope), string(events), uint16(7))
	f.Add(string(envelope), "", uint16(1))
	f.Fuzz(func(t *testing.T, envelope, events string, width uint16) {
		if len(envelope)+len(events) > 64<<10 {
			return
		}
		stride := max(int(width), (len(events)+MaxDocJobChunks-1)/MaxDocJobChunks, 1)
		input := chunkVerifierPlan([]byte(envelope), []byte(events), stride)
		raw := AssembleCanonicalDoc([]byte(envelope), [][]byte{[]byte(events)})
		var verifier CanonicalDocVerifier
		_, err := verifier.VerifyLegacyStream(context.Background(), HashContent(raw), input.Manifest, chunkVerifierLoad(input), &legacyTestStream{})
		oracle := canonicalVerificationOracle(raw)
		if (err == nil) != (oracle == nil) {
			t.Fatalf("stream=%v oracle=%v", err, oracle)
		}
	})
}
