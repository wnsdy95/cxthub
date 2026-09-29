package domain

import "time"

const CredentialApprovalFreshness = 10 * time.Minute

// AssurancePolicy limits recorded authentication evidence; it does not enable
// access enforcement, confer membership or interpret ACR/AMR as MFA.
type AssurancePolicy struct {
	EnterpriseID string `json:"enterprise_id"`
	Revision     string `json:"revision"`
	MaxAgeHours  int    `json:"max_age_hours"`
}

func DefaultAssurancePolicy(id string) AssurancePolicy {
	return AssurancePolicy{EnterpriseID: id, Revision: "assurance-default-v1", MaxAgeHours: 8}
}
func (p AssurancePolicy) Validate() error {
	if ValidateEnterpriseID(p.EnterpriseID) != nil || ValidateExternalID(p.Revision) != nil || p.MaxAgeHours < 1 || p.MaxAgeHours > 24 {
		return ErrValidation
	}
	return nil
}

// AssuranceDeadline is anchored to the original authentication. Refresh and
// explicit approval cannot turn it into a sliding timeout.
func (p AssurancePolicy) AssuranceDeadline(authenticated, browserExpiry, now time.Time, idpExpiry *time.Time) (time.Time, error) {
	if p.Validate() != nil || authenticated.IsZero() || authenticated.After(now.Add(time.Minute)) || now.Sub(authenticated) > CredentialApprovalFreshness {
		return time.Time{}, ErrUnauthorized
	}
	expires := authenticated.Add(time.Duration(p.MaxAgeHours) * time.Hour)
	if browserExpiry.Before(expires) {
		expires = browserExpiry
	}
	if idpExpiry != nil && idpExpiry.Before(expires) {
		expires = *idpExpiry
	}
	if !now.Before(expires) {
		return time.Time{}, ErrUnauthorized
	}
	return expires, nil
}

type CredentialAssurance struct {
	EnterpriseID, CredentialID, UserID string
	Proof                              FederationSession
	ApprovedAt                         time.Time
	RevokedAt                          *time.Time
}

func (a CredentialAssurance) Validate() error {
	if ValidateEnterpriseID(a.EnterpriseID) != nil || ValidateExternalID(a.CredentialID) != nil || ValidateExternalID(a.UserID) != nil || a.Proof.EnterpriseID != a.EnterpriseID || a.Proof.UserID != a.UserID || a.Proof.PolicyRevision == "" || a.Proof.DomainRevision == "" || a.ApprovedAt.IsZero() || !a.Proof.ExpiresAt.After(a.ApprovedAt) {
		return ErrValidation
	}
	if a.RevokedAt != nil && a.RevokedAt.Before(a.ApprovedAt) {
		return ErrValidation
	}
	return nil
}
