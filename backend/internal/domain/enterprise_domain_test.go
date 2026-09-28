package domain

import (
	"strings"
	"testing"
	"time"
)

func TestEnterpriseDomainExactNamesAndExpiry(t *testing.T) {
	for _, name := range []string{"https://example.test", "*.example.test", "localhost", "127.0.0.1", "[::1]", "a..test", "-a.test", "a-.test", "a/b.test", "a\u3002test", strings.Repeat("x", 64) + ".test", strings.Repeat("a.", 120) + "test"} {
		if _, err := NormalizeEnterpriseDomain(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if got, err := NormalizeEnterpriseDomain("  Sub.Example.Test. "); err != nil || got != "sub.example.test" {
		t.Fatal(got, err)
	}
	now := time.Now().UTC()
	d := EnterpriseDomain{EnterpriseID: NewID("ep_"), Domain: "example.test", Challenge: NewID("cxt-domain="), Revision: NewID("dv_"), ChallengeExpiresAt: now.Add(time.Hour)}
	if d.Validate() != nil || d.State(now) != "pending" || d.RecordName() != "_cxthub-verification.example.test" {
		t.Fatal(d)
	}
	d.VerifiedAt = now
	d.VerifiedUntil = now.Add(time.Hour)
	if d.Verified(now.Add(-time.Second)) || d.State(now) != "verified" || d.State(d.VerifiedUntil) != "expired" {
		t.Fatal("verification lifetime not enforced")
	}
}
