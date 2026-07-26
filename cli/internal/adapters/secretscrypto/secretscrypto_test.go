package secretscrypto

import "testing"

func TestRoundTrip(t *testing.T) {
	env, err := Encrypt("team-pass-123", "sk-secret-1\nhunter2\n", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt("team-pass-123", env, "sha256:abc")
	if err != nil || pt != "sk-secret-1\nhunter2\n" {
		t.Fatalf("round-trip: %q %v", pt, err)
	}
	if _, err := Decrypt("wrong-pass", env, "sha256:abc"); err == nil {
		t.Fatal("Invalid passphrase passed")
	}
	if _, err := Decrypt("team-pass-123", env, "sha256:OTHER"); err == nil {
		t.Fatal("Passphrase from another repo (AAD) passed")
	}
}

func TestValidatePassphrase(t *testing.T) {
	for _, p := range []string{"harbor twist ledger cousin", "apple bronze cedar dawn ember"} {
		if err := ValidatePassphrase(p); err != nil {
			t.Fatalf("Must be valid %q: %v", p, err)
		}
	}
	for _, p := range []string{
		"short",                       // 1 word
		"one two three",               // 3 words (<4)
		"ab cd ef gh",                 // 4 words but 11 chars (<12)
		"harbor twist ledger cousin ", // trailing space
		"harbor  twist ledger cousin", // consecutive spaces
		"Harbor Twist Ledger Cousin1", // Birtan
		"harbor twist ledger cousin1", // Number included
	} {
		if err := ValidatePassphrase(p); err == nil {
			t.Fatalf("Must reject %q", p)
		}
	}
}

func TestFingerprint(t *testing.T) {
	// JS(WebCrypto) and bytes are the same — if this vector changes, web/CLI compatibility will break (regression lock).
	if got := Fingerprint("harbor twist ledger cousin", "r1"); got != "d2d5e478ccf8" {
		t.Fatalf("Fingerprint parity vector mismatch: %q", got)
	}
	if Fingerprint("harbor twist ledger cousin", "r1") == Fingerprint("harbor twist ledger cousin", "r2") {
		t.Fatal("Repo binding failure — different repo but fingerprints match")
	}
}

func TestDecryptRejectsUnboundedKDFBeforeDerivation(t *testing.T) {
	env, err := Encrypt("harbor twist ledger cousin", "secret", "r1")
	if err != nil {
		t.Fatal(err)
	}
	env.Iterations = 2_000_000_000
	if _, err := Decrypt("harbor twist ledger cousin", env, "r1"); err == nil {
		t.Fatal("unbounded KDF iterations were accepted")
	}
	env.Iterations = Iterations
	env.NonceB64 = "AA=="
	if _, err := Decrypt("harbor twist ledger cousin", env, "r1"); err == nil {
		t.Fatal("short nonce was accepted")
	}
}
