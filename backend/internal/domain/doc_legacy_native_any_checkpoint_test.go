package domain

import (
	"context"
	"path/filepath"
	"strings"
)

type legacyNativeAnyW78CAS struct {
	directory    string
	reads        int
	writes       int
	quantum      int
	failRead     error
	failPut      error
	inputs       map[ContentHash]int
	inputWrites  int
	dependencies []legacyNativeAnyW78DependencyRead
	restoring    bool
	diagnostic   func() bool
}

type legacyNativeAnyW78DependencyRead struct {
	Hash       ContentHash `json:"hash"`
	Bytes      int         `json:"bytes"`
	Restoring  bool        `json:"restoring"`
	Diagnostic bool        `json:"diagnostic"`
}

func (storage *legacyNativeAnyW78CAS) objectPath(hash ContentHash) (string, error) {
	if ValidateContentHash(hash) != nil {
		return "", ErrIntegrity
	}
	return filepath.Join(storage.directory, strings.TrimPrefix(string(hash), "sha256:")+".cas"), nil
}

type legacyNativeAnyW78CutContext struct {
	context.Context
	cancel context.CancelFunc
	stop   func() bool
}

func (cut *legacyNativeAnyW78CutContext) Err() error {
	if cut.stop() {
		cut.cancel()
	}
	return cut.Context.Err()
}
