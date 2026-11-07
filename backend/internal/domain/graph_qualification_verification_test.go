package domain

import (
	"context"
	"errors"
	"testing"
)

func graphQualificationVerificationFixture(test *testing.T) (GraphQualificationKey, ContentHash, []GraphProjectionPart) {
	test.Helper()
	projection := graphReadQualificationTestProjection(test)
	return projection.QualificationKey(), projection.Qualification.Expected, projection.Parts
}

func TestVerifyGraphQualificationPartsSealsDecodedParts(test *testing.T) {
	key, expected, parts := graphQualificationVerificationFixture(test)
	result, err := VerifyGraphQualificationParts(context.Background(), key, expected, parts)
	if err != nil || !result.Matched() || result.GraphQualificationKey != key {
		test.Fatalf("verified result: %+v %v", result, err)
	}
	verified, prepared, err := result.VerifiedParts()
	if err != nil || !prepared || len(verified) != len(parts) {
		test.Fatalf("verified parts: %d %v %v", len(verified), prepared, err)
	}
	for index, part := range parts {
		if verified[index] != (GraphProjectionPartDigest{Kind: part.Kind, Key: part.Key, Digest: HashContent(part.Value)}) {
			test.Fatalf("part %d digest mismatch", index)
		}
	}
	// The receipt must not alias caller-owned bytes or returned slices.
	parts[0].Value[0] ^= 0xff
	verified[0].Digest = HashContent([]byte("tampered"))
	again, _, err := result.VerifiedParts()
	if err != nil || again[0].Digest == verified[0].Digest || again[0].Digest == HashContent(parts[0].Value) {
		test.Fatal("receipt aliases caller or returned state")
	}
}

func TestVerifyGraphQualificationPartsRejectsDriftAndInvalidInput(test *testing.T) {
	key, expected, parts := graphQualificationVerificationFixture(test)
	result, err := VerifyGraphQualificationParts(context.Background(), key, expected, parts)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*GraphQualificationResult){
		"expected": func(result *GraphQualificationResult) { result.Expected = HashContent([]byte("other")) },
		"actual":   func(result *GraphQualificationResult) { result.Actual = HashContent([]byte("other")) },
		"key":      func(result *GraphQualificationResult) { result.Applied.Sequence++ },
	} {
		test.Run(name, func(test *testing.T) {
			drifted := result
			mutate(&drifted)
			if _, _, err := drifted.VerifiedParts(); !errors.Is(err, ErrValidation) {
				test.Fatalf("drifted receipt accepted: %v", err)
			}
		})
	}
	plain := GraphQualificationResult{GraphQualificationKey: key, Expected: expected, Actual: result.Actual}
	if verified, prepared, err := plain.VerifiedParts(); err != nil || prepared || verified != nil {
		test.Fatalf("result without receipt must use full-validation fallback: %v %v %v", verified, prepared, err)
	}
	if _, err := VerifyGraphQualificationParts(context.Background(), GraphQualificationKey{}, expected, parts); !errors.Is(err, ErrValidation) {
		test.Fatalf("invalid key accepted: %v", err)
	}
	if _, err := VerifyGraphQualificationParts(context.Background(), key, "", parts); !errors.Is(err, ErrValidation) {
		test.Fatalf("invalid expected accepted: %v", err)
	}
	broken := append([]GraphProjectionPart(nil), parts...)
	broken[0].Value = []byte(`{}`)
	if _, err := VerifyGraphQualificationParts(context.Background(), key, expected, broken); err == nil {
		test.Fatal("undecodable parts sealed")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyGraphQualificationParts(canceled, key, expected, parts); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled verification: %v", err)
	}
}
