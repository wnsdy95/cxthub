//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacySourceHashCancellationContext struct {
	context.Context
	cancel      context.CancelFunc
	checks      int
	checkpoints [3][2]runtime.Frame
}

func (ctx *legacySourceHashCancellationContext) Err() error {
	if ctx.checks < len(ctx.checkpoints) {
		var callers [8]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for position := range ctx.checkpoints[ctx.checks] {
			frame, _ := frames.Next()
			ctx.checkpoints[ctx.checks][position] = frame
		}
	}
	ctx.checks++
	if ctx.checks == len(ctx.checkpoints) {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func legacySourceHashLargeProof(test testing.TB) domain.VerifiedSessionDoc {
	test.Helper()
	for _, testCase := range legacyMonolithicVerifierCases(test) {
		if testCase.name != "canonical" {
			continue
		}
		var document domain.CIRDocument
		if err := json.Unmarshal(testCase.raw, &document); err != nil {
			test.Fatal(err)
		}
		document.Events[0].Blocks[0].Text = strings.Repeat("original <한🙂> \"\\\n", 32<<10)
		return legacyMonolithicVerifierProof(test, document)
	}
	test.Fatal("missing canonical legacy verifier fixture")
	return domain.VerifiedSessionDoc{}
}

func TestPostgresLegacyMonolithicRangeSourceCancellationDuringInitialHash(test *testing.T) {
	expected := legacySourceHashLargeProof(test)
	canonical := expected.Bytes()
	if len(canonical) <= 4*(64<<10) {
		test.Fatal("source must span several bounded hash checkpoints")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
		test.Fatal(err)
	}
	if !json.Valid(pretty.Bytes()) || domain.HashContent(pretty.Bytes()) == expected.Hash() {
		test.Fatal("pretty source must require legacy normalization")
	}
	for _, testCase := range []struct {
		name string
		raw  []byte
	}{
		{name: "legacy-pretty-normalized-hash", raw: pretty.Bytes()},
		{name: "canonical-exact-hash", raw: canonical},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			raw := bytes.Clone(testCase.raw)
			storage := &PostgresStore{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &legacySourceHashCancellationContext{Context: ctx, cancel: cancel}
			proof, err := storage.verifyLegacyMonolithicRangeSource(observed, expected.Hash(), raw)
			if !errors.Is(err, context.Canceled) || proof != (domain.VerifiedSessionDoc{}) {
				test.Fatalf("initial hash cancellation returned nonzero=%t, error=%v", proof != (domain.VerifiedSessionDoc{}), err)
			}
			if observed.checks != len(observed.checkpoints) {
				test.Fatalf("verification continued beyond cancellation: %d context checks", observed.checks)
			}
			for _, checkpoint := range observed.checkpoints[1:] {
				if !strings.HasSuffix(checkpoint[0].Function, "domain.HashContentContext") ||
					!strings.HasSuffix(checkpoint[1].Function, "store.(*PostgresStore).verifyLegacyMonolithicRangeSource") {
					test.Fatalf("cancellation checkpoint was not in the adapter's initial hash: %q -> %q", checkpoint[0].Function, checkpoint[1].Function)
				}
			}
			previous, canceled := observed.checkpoints[1][0], observed.checkpoints[2][0]
			if previous.File != canceled.File || previous.Line != canceled.Line {
				test.Fatal("cancellation must occur at a repeated hash-loop checkpoint, not a final hash check")
			}
			if !reflect.ValueOf(&storage.docProofs).Elem().IsZero() {
				test.Fatal("initial hash cancellation changed proof or canonical-event caches")
			}
			if !bytes.Equal(raw, testCase.raw) {
				test.Fatal("canceled verification changed original source bytes")
			}
			retry, err := storage.verifyLegacyMonolithicRangeSource(context.Background(), expected.Hash(), raw)
			if err != nil || !retry.Valid() || retry.Reference() != expected.Reference() ||
				!bytes.Equal(retry.Bytes(), canonical) || domain.HashContent(retry.Bytes()) != expected.Hash() {
				test.Fatalf("uncanceled retry changed canonical bytes or identity: %v", err)
			}
			if !bytes.Equal(raw, testCase.raw) {
				test.Fatal("successful retry changed original source bytes")
			}
		})
	}
}

func TestPostgresLegacyMonolithicRangeSourceHashCompatibility(test *testing.T) {
	for _, testCase := range legacyMonolithicVerifierCases(test) {
		switch testCase.name {
		case "canonical", "canonical-v1",
			"pretty-json-normalized-hash", "noncanonical-key-order-normalized-hash", "out-of-order-events-normalized-hash",
			"pretty-json-raw-hash", "noncanonical-key-order-raw-hash", "out-of-order-events-raw-hash",
			"unsupported-version-normalized-hash", "unsupported-version-raw-hash-no-fallback":
		default:
			continue
		}
		test.Run(testCase.name, func(test *testing.T) {
			raw := bytes.Clone(testCase.raw)
			legacy, legacyErr := verifyLegacyDecodedMonolithicDoc(context.Background(), testCase.hash, raw)
			storage := &PostgresStore{}
			proof, err := storage.verifyLegacyMonolithicRangeSource(context.Background(), testCase.hash, raw)
			if !errors.Is(err, testCase.wantErr) || (err == nil) != (legacyErr == nil) {
				test.Fatalf("adapter error=%v, legacy error=%v, want %v", err, legacyErr, testCase.wantErr)
			}
			if !bytes.Equal(raw, testCase.raw) {
				test.Fatal("verification changed original source bytes")
			}
			if err != nil {
				if proof != (domain.VerifiedSessionDoc{}) {
					test.Fatal("rejected source returned a nonzero proof")
				}
				if errors.Is(err, domain.ErrUnsupportedCIRVersion) != errors.Is(testCase.wantErr, domain.ErrUnsupportedCIRVersion) ||
					errors.Is(err, domain.ErrIntegrity) != errors.Is(testCase.wantErr, domain.ErrIntegrity) {
					test.Fatalf("version/integrity error classification changed: %v", err)
				}
				if !reflect.ValueOf(&storage.docProofs).Elem().IsZero() {
					test.Fatal("rejected source admitted cached proofs")
				}
				return
			}
			if !proof.Valid() || !proof.Reference().Valid() || proof.Reference() != legacy.Reference() ||
				proof.Reference() != testCase.expected.Reference() || !bytes.Equal(proof.Bytes(), legacy.Bytes()) ||
				!bytes.Equal(proof.Bytes(), testCase.expected.Bytes()) || domain.HashContent(proof.Bytes()) != testCase.hash {
				test.Fatal("adapter proof differs from legacy normalization or typed fixture")
			}
		})
	}
}

func TestPostgresLegacyMonolithicRangeSourceGiantCanonicalString(test *testing.T) {
	expected := legacySourceHashLargeProof(test)
	raw := expected.Bytes()
	original := bytes.Clone(raw)
	storage := &PostgresStore{}
	for attempt := range 2 {
		proof, err := storage.verifyLegacyMonolithicRangeSource(context.Background(), expected.Hash(), raw)
		if err != nil || !proof.Valid() || !proof.Reference().Valid() ||
			proof.Reference() != expected.Reference() || proof.DocumentRef() != expected.DocumentRef() ||
			proof.Hash() != domain.HashContent(original) || !bytes.Equal(proof.Bytes(), original) {
			test.Fatalf("attempt %d changed giant canonical bytes or hash: %v", attempt, err)
		}
		if !bytes.Equal(raw, original) {
			test.Fatal("giant canonical verification changed original source bytes")
		}
		exported := proof.Bytes()
		exported[len(exported)/2] = '!'
		if !bytes.Equal(proof.Bytes(), original) || proof.Hash() != expected.Hash() {
			test.Fatal("exported-byte mutation changed giant canonical proof")
		}
	}
	proof, err := storage.verifyLegacyMonolithicRangeSource(context.Background(), expected.Hash(), raw)
	if err != nil {
		test.Fatal(err)
	}
	raw[len(raw)/2] = '!'
	if !proof.Valid() || !bytes.Equal(proof.Bytes(), original) || proof.Hash() != expected.Hash() {
		test.Fatal("caller-owned source mutation changed giant canonical proof")
	}
}
