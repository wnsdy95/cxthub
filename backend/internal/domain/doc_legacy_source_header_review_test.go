package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLegacySourceHeaderReviewUnsupportedDepthMatchesExactOracle(test *testing.T) {
	for _, nestedArrays := range []int{9999, 10000} {
		test.Run(fmt.Sprint(nestedArrays), func(test *testing.T) {
			header := []byte(`{"cir_version":"999","unknown":` + strings.Repeat("[", nestedArrays) + "0" + strings.Repeat("]", nestedArrays) + "}")
			want := ErrUnsupportedCIRVersion
			if nestedArrays == 10000 {
				want = ErrIntegrity
			}
			if _, err := decodeCanonicalCIREnvelope(header); !errors.Is(err, want) {
				test.Fatal("original exact-byte oracle changed its header depth boundary", err)
			}
			raw := AssembleCanonicalDoc(header, nil)
			input, output := &legacySourceProofBuffer{body: raw}, &legacySourceProofBuffer{}
			var verifier CanonicalDocVerifier
			preparation, err := NewLegacyByteSourcePreparation(context.Background(), HashContent(raw), input.input(), output, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			progress, err := legacySourceProofDrain(test, preparation)
			if want == ErrUnsupportedCIRVersion {
				if err != nil || progress != LegacyByteSourceOriginalRequired {
					test.Fatal("source helper changed unsupported-version classification at header depth 10000", progress, err)
				}
			} else if !errors.Is(err, want) || progress == LegacyByteSourceOriginalRequired {
				test.Fatal("source helper enlarged the original header depth boundary", progress, err)
			}
			if len(output.body) != 0 || len(verifier.events) != 0 {
				test.Fatal("unsupported or over-depth header published stream or semantic authority")
			}
		})
	}
}
