package domain

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"time"
)

type SAMLCertificateInfo struct {
	Fingerprint string    `json:"fingerprint"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Status      string    `json:"status"`
}

// Display information is not a trust decision. Protocol validation also checks
// the key pair and the exact signing profile before using it.
func InspectSAMLCertificate(raw string, now time.Time) (SAMLCertificateInfo, error) {
	b, rest := pem.Decode([]byte(raw))
	if b == nil || b.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return SAMLCertificateInfo{}, ErrIntegrity
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return SAMLCertificateInfo{}, ErrIntegrity
	}
	sum := sha256.Sum256(c.Raw)
	info := SAMLCertificateInfo{Fingerprint: hex.EncodeToString(sum[:]), NotBefore: c.NotBefore, NotAfter: c.NotAfter, Status: "valid"}
	switch {
	case now.Before(c.NotBefore):
		info.Status = "not_yet_valid"
	case !now.Before(c.NotAfter):
		info.Status = "expired"
	case c.NotAfter.Sub(now) <= 90*24*time.Hour:
		info.Status = "expiring"
	}
	return info, nil
}
