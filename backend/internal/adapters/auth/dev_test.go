package auth

import (
	"context"
	"testing"
)

func TestDevVerifierTokenContract(t *testing.T) {
	verifier := NewDevVerifier()

	named, err := verifier.Verify(context.Background(), "dev:alice@example.test:Alice")
	if err != nil {
		t.Fatalf("verify named token: %v", err)
	}
	if named.ID != "dev:alice@example.test" || named.Email != "alice@example.test" || named.Name != "Alice" {
		t.Fatalf("named identity = %#v", named)
	}

	unnamed, err := verifier.Verify(context.Background(), "dev:bob@example.test")
	if err != nil {
		t.Fatalf("verify unnamed token: %v", err)
	}
	if unnamed.Name != unnamed.Email {
		t.Fatalf("unnamed identity name = %q, want email %q", unnamed.Name, unnamed.Email)
	}
}
