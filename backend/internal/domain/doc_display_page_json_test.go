package domain

import (
	"fmt"
	"testing"
)

func TestMarshalDocDisplayPageExactIntegers(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	boundaries := []int{minimum, minimum + 1, -1, 0, 1, maximum - 1, maximum}
	if maximum > 1<<31-1 {
		precisionBoundary := int64(1) << 53
		for _, sign := range []int64{-1, 1} {
			for delta := int64(-2); delta <= 2; delta++ {
				boundaries = append(boundaries, int(sign*precisionBoundary+delta))
			}
		}
	}
	for boundaryIndex, boundary := range boundaries {
		test.Run(fmt.Sprintf("%d", boundary), func(test *testing.T) {
			_ = func(offset int) int {
				return boundaries[(boundaryIndex+offset)%len(boundaries)]
			}
		})
	}
}
