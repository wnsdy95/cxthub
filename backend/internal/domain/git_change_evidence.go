package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

func ValidateGitOID(oid string) error {
	if (len(oid) != 40 && len(oid) != 64) || oid != strings.ToLower(oid) || strings.Trim(oid, "0") == "" {
		return fmt.Errorf("%w: full nonzero Git object ID required", ErrValidation)
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return fmt.Errorf("%w: invalid Git object ID", ErrValidation)
	}
	return nil
}
