package domain

import (
	"fmt"
	"time"
)

const GiB int64 = 1 << 30

// StoragePolicy is provisioned by the trusted billing operator, never by clients.
// An empty Plan means measurement only until commercial entitlements are activated.
type StoragePolicy struct {
	Plan          string     `json:"plan"`
	IncludedBytes int64      `json:"included_bytes"`
	PayAsYouGo    bool       `json:"pay_as_you_go"`
	MaxBytes      *int64     `json:"max_bytes"`
	GraceBytes    int64      `json:"grace_bytes"`
	GraceUntil    *time.Time `json:"grace_until"`
}

func (p StoragePolicy) Validate() error {
	if p.Plan != "free" && p.Plan != "team" && p.Plan != "enterprise" {
		return ErrValidation
	}
	if p.IncludedBytes < 0 || p.IncludedBytes > 1<<60 || p.GraceBytes < 0 || p.GraceBytes > 1<<60 || (p.MaxBytes != nil && (*p.MaxBytes < 0 || *p.MaxBytes > 1<<60)) {
		return ErrValidation
	}
	if p.Plan == "enterprise" && (p.IncludedBytes != 50*GiB || !p.PayAsYouGo) {
		return fmt.Errorf("%w: Enterprise includes 50 GiB and meters excess storage", ErrValidation)
	}
	if p.Plan == "free" && p.PayAsYouGo {
		return ErrValidation
	}
	return nil
}

type StorageUsage struct {
	NamespaceID      string              `json:"namespace_id"`
	Policy           StoragePolicy       `json:"policy"`
	PolicyRevision   int64               `json:"policy_revision"`
	CurrentBytes     int64               `json:"current_bytes"`
	ExcessBytes      int64               `json:"excess_bytes"`
	State            string              `json:"state"`
	MeteredSince     *time.Time          `json:"metered_since"`
	PeriodStart      time.Time           `json:"period_start"`
	PeriodEnd        time.Time           `json:"period_end"`
	OverageByteHours string              `json:"overage_byte_hours"`
	Entries          []StorageUsageEntry `json:"entries"`
}
type StorageUsageEntry struct {
	Sequence   int64     `json:"sequence"`
	DeltaBytes int64     `json:"delta_bytes"`
	BytesAfter int64     `json:"bytes_after"`
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (u *StorageUsage) ProjectState(now time.Time) {
	p := u.Policy
	if p.Plan == "" {
		u.State = "metering"
		return
	}
	u.ExcessBytes = max(0, u.CurrentBytes-p.IncludedBytes)
	var cap *int64
	if p.PayAsYouGo {
		cap = p.MaxBytes
	} else {
		v := p.IncludedBytes
		if p.MaxBytes != nil {
			v = min(v, *p.MaxBytes)
		}
		cap = &v
	}
	switch {
	case cap != nil && u.CurrentBytes >= *cap:
		if p.GraceUntil != nil && now.Before(*p.GraceUntil) && u.CurrentBytes-*cap <= p.GraceBytes {
			u.State = "grace"
		} else {
			u.State = "read_only"
		}
	case u.ExcessBytes > 0 && p.PayAsYouGo:
		u.State = "overage"
	case p.IncludedBytes > 0 && u.CurrentBytes >= p.IncludedBytes-p.IncludedBytes/10:
		u.State = "warning"
	default:
		u.State = "active"
	}
}
