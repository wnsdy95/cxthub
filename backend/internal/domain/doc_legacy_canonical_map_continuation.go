package domain

import (
	"reflect"
)

type legacyCanonicalMapCapture struct {
	iterator     *reflect.MapIter
	initial      int
	expected     int
	sourceCursor int
	baseHeap     int64
	budget       int64
	ready        bool
	closed       bool
}

func (work *legacyCanonicalMapCapture) close() {
	if work != nil {
		*work = legacyCanonicalMapCapture{closed: true}
	}
}
