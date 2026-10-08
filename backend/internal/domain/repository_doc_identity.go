package domain

import "errors"

var ErrDocumentIdentityUpgradeRequired = errors.New("document identity upgrade required")
var ErrRootPublicationDisabled = errors.New("conversation root publication is disabled")

// The requirement is monotonic; an explicit empty value cannot clear opt-in.
func ValidateDocumentIdentityRequirement(current, next DocumentIdentity) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if current == DocumentIdentityRootV1 && next != current {
		return ErrConflict
	}
	return nil
}
