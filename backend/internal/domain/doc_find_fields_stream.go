package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

func BuildDocFindFieldsReader(ctx context.Context, eventHash ContentHash, eventBytes int, source io.Reader) (DocFindFields, error) {
	state, err := discoverDocFieldReader(ctx, eventHash, eventBytes, source)
	if err != nil {
		return DocFindFields{}, err
	}
	return state.FindFields()
}

func discoverDocFieldReader(ctx context.Context, eventHash ContentHash, eventBytes int, source io.Reader) (DocFieldDiscoveryState, error) {
	if err := ctx.Err(); err != nil {
		return DocFieldDiscoveryState{}, err
	}
	state, err := NewDocFieldDiscoveryState(eventHash, eventBytes)
	if err != nil || source == nil {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	digest := sha256.New()
	buffer := make([]byte, DocFieldDiscoveryStepBytes)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return DocFieldDiscoveryState{}, err
		}
		length := min(len(buffer), eventBytes-state.Next+1)
		count, readErr := source.Read(buffer[:length])
		if err := ctx.Err(); err != nil {
			return DocFieldDiscoveryState{}, err
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return DocFieldDiscoveryState{}, ErrIntegrity
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return DocFieldDiscoveryState{}, readErr
		}
		if count < 0 || count > length || count > eventBytes-state.Next {
			return DocFieldDiscoveryState{}, ErrIntegrity
		}
		if count > 0 {
			emptyReads = 0
			_, _ = digest.Write(buffer[:count])
			if err := state.consume(ctx, buffer[:count]); err != nil {
				return DocFieldDiscoveryState{}, err
			}
		} else {
			emptyReads++
			if emptyReads >= 100 && readErr == nil {
				return DocFieldDiscoveryState{}, io.ErrNoProgress
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	state.Done = state.Next == eventBytes
	state.Digest = ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
	state.HashState = nil
	if err := state.Validate(); err != nil {
		return DocFieldDiscoveryState{}, err
	}
	return state, nil
}
