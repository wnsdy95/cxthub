package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const OwnerRecoveryWindow = 10 * time.Minute

// OwnerRecovery stores only hashes. Preparation leaves a confirmed code intact;
// confirmation atomically replaces it. A redeemed code is never reusable.
type OwnerRecovery struct {
	EnterpriseID, UserID, Revision          string
	State                                   string
	ActiveHash, PendingHash, PendingSession string
	PendingUntil                            *time.Time
}

func DefaultOwnerRecovery(ep, user string) OwnerRecovery {
	return OwnerRecovery{EnterpriseID: ep, UserID: user, Revision: "recovery-default-v1", State: "unprepared"}
}
func (r OwnerRecovery) Validate() error {
	if ValidateEnterpriseID(r.EnterpriseID) != nil || ValidateExternalID(r.UserID) != nil || ValidateExternalID(r.Revision) != nil {
		return ErrValidation
	}
	switch r.State {
	case "unprepared", "ready", "consumed", "revoked":
	default:
		return ErrValidation
	}
	if (r.State == "ready") != (r.ActiveHash != "") {
		return ErrValidation
	}
	validHash := func(v string) bool {
		if len(v) != 68 || !strings.HasPrefix(v, "orh_") {
			return false
		}
		_, err := hex.DecodeString(v[4:])
		return err == nil
	}
	if r.ActiveHash != "" && !validHash(r.ActiveHash) {
		return ErrValidation
	}
	if r.PendingHash != "" {
		if !validHash(r.PendingHash) || !strings.HasPrefix(r.PendingSession, "tkh_") || ValidateStoredSessionToken(r.PendingSession) != nil || r.PendingUntil == nil || r.PendingUntil.IsZero() {
			return ErrValidation
		}
	} else if r.PendingSession != "" || r.PendingUntil != nil {
		return ErrValidation
	}
	return nil
}
func OwnerRecoveryHash(ep, user, code string) string {
	sum := sha256.Sum256([]byte("cxthub-owner-recovery-v1\x00" + ep + "\x00" + user + "\x00" + code))
	return "orh_" + hex.EncodeToString(sum[:])
}

// OwnerRepairSession is purpose-limited evidence for future identity-policy
// repair. It is NOT a federation proof or a repository/role/credential grant.
type OwnerRepairSession struct {
	EnterpriseID, UserID, SessionHash, RecoveryRevision string
	CreatedAt, ExpiresAt                                time.Time
}

func (r OwnerRepairSession) Validate() error {
	if ValidateEnterpriseID(r.EnterpriseID) != nil || ValidateExternalID(r.UserID) != nil || ValidateExternalID(r.RecoveryRevision) != nil || !strings.HasPrefix(r.SessionHash, "tkh_") || ValidateStoredSessionToken(r.SessionHash) != nil || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > OwnerRecoveryWindow {
		return ErrValidation
	}
	return nil
}
