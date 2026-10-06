package cli

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// PreparedRemoteConnection is one remote-add attempt. Its network callback is
// bound to the proposed destination; ValidateLocal only rereads local Git state.
// The command calls Connect outside locks and ValidateLocal inside config CAS.
type PreparedRemoteConnection struct {
	URL           string
	Connect       func(context.Context) (inbound.ConnectOutput, error)
	ValidateLocal func(context.Context) error
}
