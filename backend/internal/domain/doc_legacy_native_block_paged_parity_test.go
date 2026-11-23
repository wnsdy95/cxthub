package domain

import (
	"fmt"
	"testing"
)

func TestLegacyNativeBlockPagedParityPublic8And8192FlatNested(test *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, count := range []int{8, 8192} {
			test.Run(fmt.Sprintf("nested=%t/blocks=%d", nested, count), func(test *testing.T) {
				raw := legacyCanonicalBlocksPublicDocument(test, count, nested)
				if len(raw) <= ConversationManifestChunkBytes || len(raw) > 2<<20 {
					test.Fatal("public block fixture lost its original multipart/2 MiB boundaries", len(raw))
				}
			})
		}
	}
}

func TestLegacyNativeBlockPagedParityPublicSingletonAndPromotionBoundaries(test *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, count := range []int{1, 2, 9} {
			test.Run(fmt.Sprintf("nested=%t/blocks=%d", nested, count), func(test *testing.T) {
				raw := legacyCanonicalBlocksPublicDocument(test, count, nested)
				if len(raw) <= ConversationManifestChunkBytes || len(raw) > 2<<20 {
					test.Fatal("public small-list fixture lost its original multipart/2 MiB boundaries", len(raw))
				}
			})
		}
	}
}
