package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestHashContentContextPreservesIdentityAndCheckBound(t *testing.T) {
	for _, size := range []int{0, 1, canonicalCheckBytes - 1, canonicalCheckBytes, canonicalCheckBytes + 1, 2*canonicalCheckBytes + 13} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{byte(size % 251)}, size)
			original := bytes.Clone(data)
			checks := 0
			ctx := canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}
			got, err := HashContentContext(ctx, data)
			if err != nil || got != HashContent(data) || checks != (size+canonicalCheckBytes-1)/canonicalCheckBytes+1 {
				t.Fatal("identity or checkpoint interval changed", got, err, checks)
			}
			for stop := 1; stop <= checks; stop++ {
				base, cancel := context.WithCancel(context.Background())
				calls := 0
				ctx := canonicalAdmissionContext{Context: base, check: func() {
					calls++
					if calls == stop {
						cancel()
					}
				}}
				got, err := HashContentContext(ctx, data)
				cancel()
				if !errors.Is(err, context.Canceled) || got != "" {
					t.Fatal("canceled hash returned an identity", stop, got, err)
				}
			}
			if !bytes.Equal(original, data) {
				t.Fatal("hashing mutated the source")
			}
		})
	}
}
