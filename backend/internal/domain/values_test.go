package domain

import (
	"errors"
	"strings"
	"testing"
)

func validHashForTest(c string) ContentHash {
	return ContentHash("sha256:" + strings.Repeat(c, 64))
}

func TestValidateContentHash(t *testing.T) {
	valid := validHashForTest("a")
	if err := ValidateContentHash(valid); err != nil {
		t.Fatalf("valid hash rejected: %v", err)
	}
	for _, h := range []ContentHash{
		"",
		"sha256:deadbeef",
		ContentHash("sha256:" + strings.Repeat("A", 64)),
		"sha256:../../outside",
		ContentHash(strings.Repeat("a", 64)),
	} {
		if err := ValidateContentHash(h); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("expected ErrIntegrity for %q, got %v", h, err)
		}
	}
}
