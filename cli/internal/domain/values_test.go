package domain

import (
	"errors"
	"testing"
)

func TestValidateBranchNameRejectsOptionLikeNames(t *testing.T) {
	if err := ValidateBranchName("feature/safe"); err != nil {
		t.Fatalf("valid branch rejected: %v", err)
	}
	if err := ValidateBranchName("-upload-pack=evil"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("option-like branch error = %v, want ErrInvalidRef", err)
	}
}
