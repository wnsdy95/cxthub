package domain

import (
	"testing"
	"unsafe"
)

func TestLegacyCanonicalPagedReviewZeroSizedValues(test *testing.T) {
	test.Run("retained", func(test *testing.T) {
		var values legacyCanonicalPages[struct{}]
		if owned, err := values.ownedBytes(); err != nil || owned != 0 {
			test.Fatal("empty zero-sized value storage has an invalid heap estimate", owned, err)
		}
		if err := values.append(struct{}{}); err != nil {
			test.Fatal("zero-sized inline append failed", err)
		}
		if owned, err := values.ownedBytes(); err != nil || owned < 0 {
			test.Fatal("admitted zero-sized inline storage has an invalid heap estimate", owned, err)
		}
	})
	test.Run("promotion", func(test *testing.T) {
		var values legacyCanonicalPages[struct{}]
		for position := 0; position < legacyCanonicalInlineValues; position++ {
			if err := values.append(struct{}{}); err != nil {
				test.Fatal("zero-sized inline append failed", position, err)
			}
		}
		before := values
		nodeBytes := int(unsafe.Sizeof(legacyCanonicalPageNode[struct{}]{}))
		if planned, err := values.appendBytes(); err != nil || planned != nodeBytes || planned > LegacyCanonicalEncodingBytes || values != before {
			test.Fatal("zero-sized promotion did not admit only its fixed node allocation", planned, nodeBytes, err)
		}
		if err := values.append(struct{}{}); err != nil {
			test.Fatal("bounded zero-sized value promotion failed", err)
		}
		if values.length() != legacyCanonicalInlineValues+1 || values.at(legacyCanonicalInlineValues) == nil {
			test.Fatal("zero-sized promotion lost its appended value", values.length())
		}
		if owned, err := values.ownedBytes(); err != nil || owned < int64(nodeBytes) {
			test.Fatal("zero-sized promoted storage lost its actual node allocation", owned, nodeBytes, err)
		}
	})
}
