package domain

import (
	"context"
	"errors"
)

type canonicalFrameTokenState struct {
	stack   [10000]byte
	depth   int
	quoted  bool
	escaped bool
}

func (state *canonicalFrameTokenState) add(current byte) (complete bool, err error) {
	if state.quoted {
		if state.escaped {
			state.escaped = false
		} else if current == '\\' {
			state.escaped = true
		} else if current == '"' {
			state.quoted = false
		}
		return false, nil
	}
	switch current {
	case '"':
		state.quoted = true
	case '{', '[':
		if state.depth == len(state.stack) {
			return false, ErrIntegrity
		}
		state.stack[state.depth] = current
		state.depth++
	case '}', ']':
		if state.depth == 0 || (current == '}' && state.stack[state.depth-1] != '{') || (current == ']' && state.stack[state.depth-1] != '[') {
			return false, ErrIntegrity
		}
		state.depth--
		return state.depth == 0, nil
	}
	return false, nil
}

func canonicalVerificationFrame(ctx context.Context, raw []byte) ([]byte, []byte, error) {
	return canonicalFrameParts(ctx, raw)
}

func canonicalFrameParts[Raw ~string | ~[]byte](ctx context.Context, raw Raw) (Raw, Raw, error) {
	var empty Raw
	if err := ctx.Err(); err != nil {
		return empty, empty, err
	}
	if len(raw) < len(canonicalDocPrefix)+len(canonicalDocSuffix) || string(raw[:len(canonicalDocPrefix)]) != canonicalDocPrefix || string(raw[len(raw)-len(canonicalDocSuffix):]) != canonicalDocSuffix {
		return empty, empty, ErrIntegrity
	}
	start := len(canonicalDocPrefix)
	if raw[start] != '{' {
		return empty, empty, ErrIntegrity
	}
	var state canonicalFrameTokenState
	for offset := start; offset < len(raw)-len(canonicalDocSuffix); offset++ {
		if (offset-start)%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return empty, empty, err
			}
		}
		complete, err := state.add(raw[offset])
		if err != nil {
			return empty, empty, err
		}
		if complete {
			end := offset + 1
			if len(raw)-end < len(canonicalDocMiddle) || string(raw[end:end+len(canonicalDocMiddle)]) != canonicalDocMiddle {
				return empty, empty, ErrIntegrity
			}
			streamStart := end + len(canonicalDocMiddle)
			streamEnd := len(raw) - len(canonicalDocSuffix)
			if streamStart > streamEnd {
				return empty, empty, ErrIntegrity
			}
			return raw[start:end], raw[streamStart:streamEnd], ctx.Err()
		}
	}
	return empty, empty, ErrIntegrity
}

func canonicalVerificationFailure(ctx context.Context, raw []byte, err error) error {
	if !errors.Is(err, ErrUnsupportedCIRVersion) {
		return err
	}
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	_, _, ok := canonicalStream(raw)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if !ok {
		return ErrIntegrity
	}
	return err
}
