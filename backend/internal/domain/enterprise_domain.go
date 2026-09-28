package domain

import (
	"net"
	"strings"
	"time"
)

const EnterpriseDomainChallengeTTL = 24 * time.Hour
const EnterpriseDomainVerificationTTL = 30 * 24 * time.Hour
const MaxEnterpriseDomains = 20
const enterpriseDomainRecordPrefix = "_cxthub-verification."

// EnterpriseDomain is an observation of DNS control, not an identity or access
// grant. The exact name never authorizes its parents, children or email users.
type EnterpriseDomain struct {
	EnterpriseID       string    `json:"enterprise_id"`
	Domain             string    `json:"domain"`
	Challenge          string    `json:"challenge"`
	Revision           string    `json:"revision"`
	ChallengeExpiresAt time.Time `json:"challenge_expires_at"`
	VerifiedAt         time.Time `json:"verified_at"`
	VerifiedUntil      time.Time `json:"verified_until"`
}

func NormalizeEnterpriseDomain(name string) (string, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if len(name) > 253-len(enterpriseDomainRecordPrefix) || net.ParseIP(name) != nil || !strings.Contains(name, ".") {
		return "", ErrValidation
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrValidation
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", ErrValidation
			}
		}
	}
	return name, nil
}

func (d EnterpriseDomain) Validate() error {
	if ValidateEnterpriseID(d.EnterpriseID) != nil {
		return ErrValidation
	}
	normalized, err := NormalizeEnterpriseDomain(d.Domain)
	if err != nil || normalized != d.Domain || validatePrefixedHexID(d.Challenge, "cxt-domain=", 32) != nil || validatePrefixedHexID(d.Revision, "dv_", 32) != nil || d.ChallengeExpiresAt.IsZero() {
		return ErrValidation
	}
	if d.VerifiedAt.IsZero() != d.VerifiedUntil.IsZero() || !d.VerifiedAt.IsZero() && !d.VerifiedUntil.After(d.VerifiedAt) {
		return ErrValidation
	}
	return nil
}
func (d EnterpriseDomain) RecordName() string { return enterpriseDomainRecordPrefix + d.Domain }
func (d EnterpriseDomain) Verified(now time.Time) bool {
	return !d.VerifiedAt.IsZero() && !now.Before(d.VerifiedAt) && now.Before(d.VerifiedUntil)
}
func (d EnterpriseDomain) State(now time.Time) string {
	if d.Verified(now) {
		return "verified"
	}
	if !d.VerifiedAt.IsZero() || !now.Before(d.ChallengeExpiresAt) {
		return "expired"
	}
	return "pending"
}
