package domain

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func archivedVerifiedHeaderFrame(ctx context.Context, raw []byte) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !bytes.HasPrefix(raw, []byte(canonicalDocPrefix)) || !bytes.HasSuffix(raw, []byte(canonicalDocSuffix)) {
		return nil, nil, ErrIntegrity
	}
	start := len(canonicalDocPrefix)
	if raw[start] != '{' {
		return nil, nil, ErrIntegrity
	}
	var stack [10000]byte
	depth := 0
	quoted, escaped := false, false
	for offset := start; offset < len(raw)-len(canonicalDocSuffix); offset++ {
		if (offset-start)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		current := raw[offset]
		if quoted {
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				quoted = false
			}
			continue
		}
		switch current {
		case '"':
			quoted = true
		case '{', '[':
			if depth == len(stack) {
				return nil, nil, ErrIntegrity
			}
			stack[depth] = current
			depth++
		case '}', ']':
			if depth == 0 || (current == '}' && stack[depth-1] != '{') || (current == ']' && stack[depth-1] != '[') {
				return nil, nil, ErrIntegrity
			}
			depth--
			if depth == 0 {
				end := offset + 1
				if !bytes.HasPrefix(raw[end:], []byte(canonicalDocMiddle)) {
					return nil, nil, ErrIntegrity
				}
				streamStart := end + len(canonicalDocMiddle)
				streamEnd := len(raw) - len(canonicalDocSuffix)
				if streamStart > streamEnd {
					return nil, nil, ErrIntegrity
				}
				return raw[start:end], raw[streamStart:streamEnd], ctx.Err()
			}
		}
	}
	return nil, nil, ErrIntegrity
}

func TestCanonicalFrameArchivedRepresentationParity(test *testing.T) {
	full := []byte(`{"envelope":{"model":"` + strings.Repeat("x", 2*canonicalCheckBytes+7) + `\"},\"events\":[🙂"},"events":[{"kind":"turn","seq":1}]}`)
	inputs := [][]byte{
		nil, {}, []byte(canonicalDocPrefix), []byte(`{"envelope":]}`), []byte(`{"envelope":{},"events":[]}`),
		[]byte(`{"envelope":{},"events":[,]}`), []byte(`{"envelope":[{}],"events":[]}`), []byte(`{"envelope":{"a":[}]},"events":[]}`),
		[]byte(`{"envelope":{},"events": [ ]}`), append(bytes.Clone(full), '\n'), full,
	}
	for _, end := range []int{1, len(canonicalDocPrefix) - 1, len(canonicalDocPrefix) + 1, canonicalCheckBytes - 1, canonicalCheckBytes, canonicalCheckBytes + 1, len(full) - 3, len(full) - 1} {
		inputs = append(inputs, full[:end])
	}
	for _, depth := range []int{9998, 9999, 10000, 10001} {
		inputs = append(inputs, []byte(`{"envelope":{"a":`+strings.Repeat("[", depth)+"0"+strings.Repeat("]", depth)+`},"events":[]}`))
	}
	for inputIndex, raw := range inputs {
		for _, cancelAt := range []int{0, 1, 2, 3, 4, 5, 6} {
			newContext := func() (context.Context, context.CancelFunc) {
				base, cancel := context.WithCancel(context.Background())
				checks := 0
				return canonicalAdmissionContext{Context: base, check: func() {
					checks++
					if checks == cancelAt {
						cancel()
					}
				}}, cancel
			}
			before, cancelBefore := newContext()
			wantEnvelope, wantStream, wantErr := archivedVerifiedHeaderFrame(before, raw)
			cancelBefore()
			after, cancelAfter := newContext()
			gotEnvelope, gotStream, gotErr := canonicalVerificationFrame(after, raw)
			cancelAfter()
			textContext, cancelText := newContext()
			textEnvelope, textStream, textErr := canonicalFrameParts(textContext, string(raw))
			cancelText()
			if wantErr != gotErr || wantErr != textErr || !bytes.Equal(wantEnvelope, gotEnvelope) || !bytes.Equal(wantStream, gotStream) || (wantEnvelope == nil) != (gotEnvelope == nil) || (wantStream == nil) != (gotStream == nil) || string(wantEnvelope) != textEnvelope || string(wantStream) != textStream {
				test.Fatalf("framing semantics changed: input=%d cancel_at=%d want=%v bytes=%v text=%v", inputIndex, cancelAt, wantErr, gotErr, textErr)
			}
		}
	}
}
