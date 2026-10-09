package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// FrozenSessionID reads only the first verified private chunk, never SourcePath.
// The fixed 1 MiB chunk boundary is also the header byte budget. This is an
// identity hint for freezing memory, not a substitute for ProjectFrozen's full
// chunk, input-hash and CIR verification.
func (s *SessionCaptureAdapter) FrozenSessionID(ctx context.Context, root string, in domain.FrozenCaptureInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := in.Validate(); err != nil {
		return "", err
	}
	dir, err := openFrozenDir(root, false)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	chunk := in.Chunks[0]
	raw := make([]byte, int(chunk.Size))
	if err := readFrozenChunk(ctx, dir, chunk, raw); err != nil {
		return "", err
	}
	if len(in.Chunks) == 1 && in.Hash != chunk.Hash {
		return "", domain.ErrFrozenCaptureInput
	}
	for n := 0; n < 512 && len(raw) > 0; n++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line, rest, newline := bytes.Cut(raw, []byte{'\n'})
		raw = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !newline && in.Size > chunk.Size {
			// Large first records can put the complete identity before a body
			// that crosses the budget. Decode only the verified prefix; EOF
			// without an identity is unavailable, never proof of memory absence.
			id, err := frozenHeaderPrefixSessionID(line, in.Provider)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", ctx.Err()
			}
			if err != nil {
				return "", fmt.Errorf("%w: malformed frozen session header", domain.ErrFrozenCaptureInput)
			}
			return id, ctx.Err()
		}
		id, err := frozenHeaderSessionID(line, in.Provider)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, ctx.Err()
		}
	}
	return "", ctx.Err()
}

// This decoder can see at most one already-verified chunk. Unknown values are
// skipped as raw JSON, without building a provider document or reading its tail.
// Complete records retain the stricter whole-record validation below.
func frozenHeaderPrefixSessionID(prefix []byte, provider domain.ProviderKind) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if provider == domain.ProviderClaude {
		return frozenObjectIdentity(dec, "sessionId")
	}
	start, err := dec.Token()
	if err != nil {
		return "", err
	}
	if start != json.Delim('{') {
		return "", domain.ErrFrozenCaptureInput
	}
	var kind string
	var payload json.RawMessage
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch key {
		case "type":
			value, err := dec.Token()
			if err != nil {
				return "", err
			}
			var ok bool
			kind, ok = value.(string)
			if !ok {
				return "", domain.ErrFrozenCaptureInput
			}
			if kind != "session_meta" {
				return "", nil
			}
			if payload != nil {
				return frozenObjectIdentity(json.NewDecoder(bytes.NewReader(payload)), "id")
			}
		case "payload":
			if kind == "session_meta" {
				return frozenObjectIdentity(dec, "id")
			}
			// JSON member order is not significant. Retain a preceding payload
			// only if it fits; an ID without a verified type is insufficient.
			if err := dec.Decode(&payload); err != nil {
				return "", err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return "", err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return "", err
	}
	if kind == "session_meta" {
		return "", domain.ErrFrozenCaptureInput // complete header lacks payload
	}
	return "", nil
}

func frozenObjectIdentity(dec *json.Decoder, field string) (string, error) {
	start, err := dec.Token()
	if err != nil {
		return "", err
	}
	if start != json.Delim('{') {
		return "", domain.ErrFrozenCaptureInput
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", err
		}
		if key == field {
			value, err := dec.Token()
			if err != nil {
				return "", err
			}
			id, ok := value.(string)
			if !ok || (id != "" && !validHookSessionID(id)) {
				return "", domain.ErrFrozenCaptureInput
			}
			return id, nil
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", err
		}
	}
	_, err = dec.Token()
	return "", err
}

func frozenHeaderSessionID(line []byte, provider domain.ProviderKind) (string, error) {
	invalid := fmt.Errorf("%w: malformed frozen session header", domain.ErrFrozenCaptureInput)
	var row map[string]json.RawMessage
	if err := json.Unmarshal(line, &row); err != nil || row == nil {
		return "", invalid
	}
	value := row["sessionId"]
	if provider == domain.ProviderCodex {
		var kind string
		if raw, ok := row["type"]; ok {
			if err := json.Unmarshal(raw, &kind); err != nil || bytes.Equal(raw, []byte("null")) {
				return "", invalid
			}
		}
		if kind != "session_meta" {
			return "", nil
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(row["payload"], &payload); err != nil || payload == nil {
			return "", invalid
		}
		value = payload["id"]
	}
	if value == nil {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(value, &id); err != nil || bytes.Equal(value, []byte("null")) {
		return "", invalid
	}
	if id != "" && !validHookSessionID(id) {
		return "", invalid
	}
	return id, nil
}

// ScrubFrozenMemory sanitizes a copy of the receipt, without reading provider
// files or processing the transcript again. Policy checks bracket all reads of
// the existing scrub configuration, including explicit absence of memory.
func (s *SessionCaptureAdapter) ScrubFrozenMemory(ctx context.Context, root string, in domain.FrozenCaptureInput) (*domain.NativeMemory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if err := checkFrozenPolicy(ctx, root, in.PolicyFingerprint); err != nil {
		return nil, err
	}
	if in.NativeMemory == nil {
		return nil, nil
	}
	tier := LoadScrubOptions(root).Tier
	mask := func(value string) string {
		clean, _ := ScrubSecrets([]byte(value), root)
		if tier == ScrubOff {
			return string(clean)
		}
		return maskString(string(clean), tier)
	}
	out := *in.NativeMemory
	out.Text = mask(out.Text)
	out.AutoLoadedPrefix = mask(out.AutoLoadedPrefix)
	if out.Structured != nil {
		out.Structured = make(map[string]string, len(in.NativeMemory.Structured))
		for key, value := range in.NativeMemory.Structured {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			out.Structured[key] = mask(value)
		}
	}
	if err := checkFrozenPolicy(ctx, root, in.PolicyFingerprint); err != nil {
		return nil, err
	}
	return &out, nil
}
