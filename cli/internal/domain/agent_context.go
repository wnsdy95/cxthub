package domain

import (
	"errors"
	"strings"
)

var ErrContextBudgetExceeded = errors.New("context_budget_exceeded")
var ErrProviderCapabilityUnknown = errors.New("provider_capability_unknown")

// IsAgentContextPackageText lets capture/distillation exclude synthetic input
// from new user decisions. Archived source records remain untouched.
func IsAgentContextPackageText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[cxt context package v1]\n")
}
