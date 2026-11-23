package domain

import (
	"context"
	"reflect"
)

type legacyCanonicalMapContinuationValueCodec interface {
	check(context.Context, int64) error
}

type legacyCanonicalMapContinuationEntry struct {
}

type legacyCanonicalMapContinuationState struct {
	Kind     string      `json:"kind"`
	Version  uint8       `json:"version"`
	Binding  ContentHash `json:"binding"`
	Count    int         `json:"count"`
	Position int         `json:"position"`
}

type legacyCanonicalMapContinuationCheckpoint struct {
	iterator   reflect.MapIter
	codec      legacyCanonicalMapContinuationValueCodec
	state      legacyCanonicalMapContinuationState
	entry      legacyCanonicalMapContinuationEntry
	budget     int64
	frameIndex int
	position   int
	phase      uint8
	ready      bool
	closed     bool
	failure    error
}
