package domain

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestLegacyDocRangeGenerationRootVersion2WireCompatibility(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			generation, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, count))
			if generation.Version != 2 || generation.SourceIdentity != nil || generation.Identity() != DocumentIdentityRootV1 {
				test.Fatal("root generation changed its implicit version 2 identity")
			}
			encoded, err := generation.Encode()
			if err != nil || bytes.Contains(encoded, []byte(`"source_identity"`)) {
				test.Fatalf("version 2 acquired source_identity: %s, %v", encoded, err)
			}
			identity := DocumentIdentityLegacy
			generation.SourceIdentity = &identity
			if err := generation.Validate(generation.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("version 2 accepted an explicit legacy source identity: %v", err)
			}
		})
	}
}
