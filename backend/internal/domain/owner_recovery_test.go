package domain

import (
	"testing"
	"time"
)

func TestOwnerRecoveryRejectsInconsistentStoredState(t *testing.T) {
	ep, user := NewID("ep_"), NewID("user_")
	initial := DefaultOwnerRecovery(ep, user)
	if err := initial.Validate(); err != nil {
		t.Fatal(err)
	}
	hash := OwnerRecoveryHash(ep, user, "synthetic-secret")
	if hash == OwnerRecoveryHash(ep, "other", "synthetic-secret") || hash == OwnerRecoveryHash("other", user, "synthetic-secret") {
		t.Fatal("recovery hash not identity-bound")
	}
	deadline := time.Now().Add(OwnerRecoveryWindow)
	pending := initial
	pending.PendingHash, pending.PendingSession, pending.PendingUntil = hash, HashToken("browser"), &deadline
	if err := pending.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*OwnerRecovery){
		func(r *OwnerRecovery) { r.State = "ready" },
		func(r *OwnerRecovery) { r.ActiveHash = hash },
		func(r *OwnerRecovery) { r.PendingHash = "" },
		func(r *OwnerRecovery) { r.PendingSession = "tkh_bad" },
		func(r *OwnerRecovery) { r.PendingUntil = nil },
	} {
		r := pending
		change(&r)
		if r.Validate() == nil {
			t.Fatal("inconsistent recovery accepted")
		}
	}
	lease := OwnerRepairSession{EnterpriseID: ep, UserID: user, SessionHash: HashToken("browser"), RecoveryRevision: "or_revision", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := lease.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*OwnerRepairSession){
		func(r *OwnerRepairSession) { r.SessionHash = "tkh_bad" },
		func(r *OwnerRepairSession) { r.ExpiresAt = r.CreatedAt },
		func(r *OwnerRepairSession) { r.ExpiresAt = r.CreatedAt.Add(OwnerRecoveryWindow + time.Second) },
	} {
		r := lease
		change(&r)
		if r.Validate() == nil {
			t.Fatal("invalid repair lease accepted")
		}
	}
}
