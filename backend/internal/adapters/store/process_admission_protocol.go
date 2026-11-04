package store

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const processAdmissionProtocolVersion = 7

func ProcessAdmissionFingerprint(budgetFingerprint string) (string, error) {
	if len(budgetFingerprint) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return "", fmt.Errorf("%w: invalid process budget fingerprint", domain.ErrValidation)
	}
	digest, err := base64.RawURLEncoding.DecodeString(budgetFingerprint)
	if err != nil || len(digest) != sha256.Size || base64.RawURLEncoding.EncodeToString(digest) != budgetFingerprint {
		return "", fmt.Errorf("%w: invalid process budget fingerprint", domain.ErrValidation)
	}
	contract := fmt.Sprintf("cxthub/process-admission/v%d\x00%s\x00%s", processAdmissionProtocolVersion, releasedMigrationCatalogSHA256, budgetFingerprint)
	identity := sha256.Sum256([]byte(contract))
	return base64.RawURLEncoding.EncodeToString(identity[:]), nil
}
