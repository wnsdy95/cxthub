package outbound

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RemoteRepairPlan deliberately changes exactly one pointer. Multi-ref repair
// needs a separate transaction and cannot be emulated by looping over --force.
// PreservedMemory contains the losing immutable digest so local object cleanup
// cannot erase the recovery evidence held by an accepted receipt.
type RemoteRepairPlan struct {
	Version         int                  `json:"version"`
	ID              domain.ContentHash   `json:"id"`
	RepoID          string               `json:"repo_id"`
	Remote          string               `json:"remote"`
	Reason          string               `json:"reason"`
	Observation     domain.ContentHash   `json:"observation"`
	Expected        CheckoutState        `json:"expected"`
	BeforeRef       *domain.Ref          `json:"before_ref,omitempty"`
	AfterRef        *domain.Ref          `json:"after_ref,omitempty"`
	Snapshot        domain.ContentHash   `json:"snapshot,omitempty"`
	BeforeMemory    domain.ContentHash   `json:"before_memory,omitempty"`
	AfterMemory     domain.ContentHash   `json:"after_memory,omitempty"`
	PreservedMemory *domain.MemoryDigest `json:"preserved_memory,omitempty"`
}

type RemoteRepairReceipt struct {
	Plan      RemoteRepairPlan `json:"plan"`
	AppliedAt time.Time        `json:"applied_at"`
}

func RemoteRepairPlanID(plan RemoteRepairPlan) domain.ContentHash {
	plan.ID = ""
	raw, _ := json.Marshal(plan)
	return domain.HashContent(raw)
}

// RemoteRepairStore accepts only a previewed exact-pointer transition. A
// durable journal fences later writers; recovery must not rewind newer writes.
type RemoteRepairStore interface {
	ReadCheckoutState(context.Context, string) (CheckoutState, error)
	ReadRemoteObservation(context.Context, string, string) (RemoteObservation, error)
	ReadRemoteRepairReceipt(context.Context, domain.ContentHash) (RemoteRepairReceipt, error)
	ApplyRemoteRepair(context.Context, RemoteRepairPlan) (RemoteRepairReceipt, error)
}
