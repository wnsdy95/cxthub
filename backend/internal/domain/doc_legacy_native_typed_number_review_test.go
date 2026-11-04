package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeTypedNumberReviewSourceAbortKeepsNilPointer(test *testing.T) {
	for _, diagnostics := range []legacyNativeNumberDiagnostics{legacyNativeNumberDiagnosticsFull, legacyNativeNumberDiagnosticsRejectOnly} {
		for _, failure := range []string{"source-error", "source-hash", "cancel-during-read", "cancel-after-yield"} {
			name := "full/" + failure
			if diagnostics == legacyNativeNumberDiagnosticsRejectOnly {
				name = "reject-only/" + failure
			}
			test.Run(name, func(test *testing.T) {
				model := &ProviderMetadata{}
				metadata := newLegacyNativeObjectFields(reflect.TypeFor[ProviderMetadata]())
				native, err := newLegacyNativeObject(reflect.ValueOf(model), &metadata, 1, false)
				if err != nil {
					test.Fatal("typed number review fixture failed", err)
				}
				fieldIndex := -1
				for index, field := range metadata.fields {
					if field.name == "create_time" {
						fieldIndex = index
						break
					}
				}
				if fieldIndex < 0 {
					test.Fatal("typed number review fixture has no create_time field")
				}
				token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
				native.field, native.phase = fieldIndex, legacyNativeObjectApply
				native.valueStart, native.valueEnd = 0, len(token)
				native.position, native.end, native.valueFirst = len(token), len(token), token[0]
				buffer := &legacySourceProofBuffer{body: []byte(token)}
				source := buffer.input()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				read := source.Read
				readCalls := 0
				failureError := errors.New("typed number review source read failed")
				source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
					readCalls++
					switch failure {
					case "source-error":
						return nil, failureError
					case "source-hash":
						body, readError := read(readContext, offset, length)
						if readError != nil {
							return nil, readError
						}
						corrupted := append([]byte(nil), body...)
						corrupted[0] = '2'
						return corrupted, nil
					case "cancel-during-read":
						body, readError := read(readContext, offset, length)
						cancel()
						return body, readError
					default:
						return read(readContext, offset, length)
					}
				}
				if failure == "cancel-after-yield" {
					partial := &legacyNormalizationInput{
						ctx: ctx, source: source, numberDiagnostics: diagnostics,
						workBytes: ConversationManifestChunkBytes - 4096,
					}
					complete, partialError := native.decodeTypedNumber(ctx, partial)
					if complete || partialError != nil || partial.failure != nil || readCalls != 1 ||
						partial.unbounded || partial.workBytes > ConversationManifestChunkBytes || partial.loads > 2 {
						test.Fatal("typed number review fixture did not pause within its shared budget", complete, partialError)
					}
					cancel()
				}
				input := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: diagnostics}
				complete, decodeError := native.decodeTypedNumber(ctx, input)
				if complete || decodeError == nil || errors.Is(decodeError, errLegacyNativeNumberRejected) {
					test.Fatal("typed number converted a source or cancellation failure into a semantic result", complete, decodeError)
				}
				switch failure {
				case "source-error":
					if !errors.Is(decodeError, failureError) || !errors.Is(decodeError, ErrPreparedDocRangeObject) || input.failure == nil {
						test.Fatal("typed number lost its source read failure", decodeError)
					}
				case "source-hash":
					if !errors.Is(decodeError, ErrPreparedDocRangeObject) || input.failure == nil {
						test.Fatal("typed number accepted a failed authenticated source part", decodeError)
					}
				default:
					if !errors.Is(decodeError, context.Canceled) {
						test.Fatal("typed number lost its cancellation failure", decodeError)
					}
				}
				if readCalls != 1 || input.unbounded || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
					test.Fatal("typed number escaped source-abort work or load limits", readCalls, input.workBytes, input.loads)
				}
				if model.CreateTime != nil {
					test.Fatal("typed number published a nil pointee before an authenticated source read or cancellation completed", failure)
				}
			})
		}
	}
}
