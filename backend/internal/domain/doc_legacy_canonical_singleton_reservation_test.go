package domain

import (
	"testing"
	"unsafe"
)

func TestLegacyCanonicalFirstBlockReservationMatchesOneValue(test *testing.T) {
	var records legacyCanonicalPages[legacyNativeBlockValue]
	reserved, err := records.appendBytes()
	if err != nil || reserved != int(unsafe.Sizeof(legacyNativeBlockValue{})) {
		test.Fatal("first native block record reserved unused inline slots", reserved, unsafe.Sizeof(legacyNativeBlockValue{}), err)
	}
}
