package store

import (
	"errors"
	"math"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootPublicationHeapIncludesOwnedEstimateOnce(test *testing.T) {
	var child, parent rootPublicationHeapEstimate
	child.allocation(1<<20, 1)
	parent.allocation(1, 128)
	before := parent.bytes
	parent.include(child)
	if parent.err != nil || parent.bytes != before+child.bytes || child.bytes != 64+2*(1<<20) {
		test.Fatal("aggregate reapplied allocation overhead to an already guarded owner", parent, child)
	}
}

func TestRootPublicationHeapIncludeFailsClosed(test *testing.T) {
	for _, pair := range [][2]int64{{-1, 0}, {0, -1}, {math.MaxInt64, 1}} {
		parent := rootPublicationHeapEstimate{bytes: pair[0]}
		parent.include(rootPublicationHeapEstimate{bytes: pair[1]})
		if !errors.Is(parent.err, domain.ErrIntegrity) || parent.bytes != pair[0] {
			test.Fatal("invalid aggregate estimate was admitted", pair, parent)
		}
	}
	parent := rootPublicationHeapEstimate{}
	parent.include(rootPublicationHeapEstimate{err: domain.ErrConflict})
	if !errors.Is(parent.err, domain.ErrConflict) || parent.bytes != 0 {
		test.Fatal("child accounting failure disappeared at the owner boundary", parent)
	}
}
