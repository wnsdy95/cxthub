package domain

import (
	"fmt"
	"testing"
)

type legacyCanonicalNativeProgressFailure string

type legacyCanonicalNativeProgressProbe struct {
	testing.TB
}

func (probe legacyCanonicalNativeProgressProbe) Fatal(arguments ...any) {
	panic(legacyCanonicalNativeProgressFailure(fmt.Sprint(arguments...)))
}
