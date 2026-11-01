package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

func canonicalEventProjection(ctx context.Context, raw []byte) ([]byte, error) {
	if len(raw) <= canonicalCheckBytes {
		return raw, ctx.Err()
	}
	var projected []byte
	copied, depth, protected := 0, 0, -1
	for offset := 0; offset < len(raw); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := canonicalProjectionStringStart(ctx, raw, offset, &depth)
		if err != nil {
			if errors.Is(err, ErrIntegrity) {
				return raw, ctx.Err()
			}
			return nil, err
		}
		offset = next
		if offset == len(raw) {
			break
		}
		end, err := canonicalStringEnd(ctx, raw, offset)
		if err != nil {
			return nil, err
		}
		keyEnd, err := canonicalProjectionSkipSpace(ctx, raw, end)
		if err != nil {
			return nil, err
		}
		isKey := keyEnd < len(raw) && raw[keyEnd] == ':'
		if isKey && end-offset <= 2+6*len("create_time") {
			name := raw[offset+1 : end-1]
			sensitive := bytes.EqualFold(name, []byte("create_time"))
			if !sensitive && bytes.IndexByte(name, '\\') >= 0 {
				var decoded string
				sensitive = json.Unmarshal(raw[offset:end], &decoded) == nil && strings.EqualFold(decoded, "create_time")
			}
			if sensitive {
				protected, err = canonicalProjectionSkipSpace(ctx, raw, keyEnd+1)
				if err != nil {
					return nil, err
				}
			}
		}
		if isKey && (bytes.Equal(raw[offset:end], []byte(`"input"`)) || bytes.Equal(raw[offset:end], []byte(`"output"`))) {
			start, err := canonicalProjectionSkipSpace(ctx, raw, keyEnd+1)
			if err != nil {
				return nil, err
			}
			payloadEnd, marker, err := canonicalPayloadProjection(ctx, raw, start, 10000-depth)
			if err != nil {
				if errors.Is(err, ErrIntegrity) {
					return raw, ctx.Err()
				}
				return nil, err
			}
			projected = append(projected, raw[copied:start]...)
			projected = append(projected, marker...)
			copied, offset = payloadEnd, payloadEnd
			continue
		}
		if end-offset > canonicalCheckBytes && !isKey && offset != protected {
			if err := verifyCanonicalStringFragments(ctx, raw[offset+1:end-1]); err != nil {
				if errors.Is(err, ErrIntegrity) {
					return raw, ctx.Err()
				}
				return nil, err
			}
			projected = append(projected, raw[copied:offset]...)
			projected = append(projected, '"', 'x', '"')
			copied = end
		}
		offset = end
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if projected == nil {
		return raw, nil
	}
	return append(projected, raw[copied:]...), nil
}

func canonicalProjectionStringStart(ctx context.Context, raw []byte, offset int, depth *int) (int, error) {
	start := offset
	for offset < len(raw) && raw[offset] != '"' {
		if (offset-start)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		switch raw[offset] {
		case '{', '[':
			*depth++
			if *depth > 10000 {
				return 0, ErrIntegrity
			}
		case '}', ']':
			*depth--
			if *depth < 0 {
				return 0, ErrIntegrity
			}
		}
		offset++
	}
	return offset, ctx.Err()
}

func canonicalProjectionSkipSpace(ctx context.Context, raw []byte, offset int) (int, error) {
	start := offset
	for offset < len(raw) && (raw[offset] == ' ' || raw[offset] == '\n' || raw[offset] == '\r' || raw[offset] == '\t') {
		if (offset-start)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		offset++
	}
	return offset, ctx.Err()
}

func canonicalStringEnd(ctx context.Context, raw []byte, start int) (int, error) {
	escaped := false
	for offset := start + 1; offset < len(raw); offset++ {
		if (offset-start-1)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if escaped {
			escaped = false
		} else if raw[offset] == '\\' {
			escaped = true
		} else if raw[offset] == '"' {
			return offset + 1, nil
		}
	}
	return 0, ErrIntegrity
}

func verifyCanonicalStringFragments(ctx context.Context, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start, digits := 0, 0
	escaped := false
	for offset, current := range content {
		if offset-start >= canonicalCheckBytes-10 && !escaped && digits == 0 && utf8.RuneStart(current) {
			if err := verifyCanonicalStringFragment(ctx, content[start:offset]); err != nil {
				return err
			}
			start = offset
		}
		if offset-start >= canonicalCheckBytes-2 {
			return ErrIntegrity
		}
		if digits > 0 {
			digits--
		} else if escaped {
			escaped = false
			if current == 'u' {
				digits = 4
			}
		} else if current == '\\' {
			escaped = true
		}
	}
	return verifyCanonicalStringFragment(ctx, content[start:])
}

func verifyCanonicalStringFragment(ctx context.Context, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	quoted := make([]byte, 0, len(content)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, content...)
	quoted = append(quoted, '"')
	if _, valid := canonicalStringValue(quoted); !valid {
		return ErrIntegrity
	}
	return ctx.Err()
}

func canonicalStringValue(raw []byte) (string, bool) {
	var decoded string
	if json.Unmarshal(raw, &decoded) != nil {
		return "", false
	}
	canonical, err := json.Marshal(decoded)
	return decoded, err == nil && bytes.Equal(raw, canonical)
}
