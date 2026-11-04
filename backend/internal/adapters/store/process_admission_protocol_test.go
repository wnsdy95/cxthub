package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestProcessAdmissionFingerprintBindsProtocolSchemaAndBudget(test *testing.T) {
	budget := base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	got, err := ProcessAdmissionFingerprint(budget)
	witness := sha256.Sum256([]byte("cxthub/process-admission/v7\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget))
	want := base64.RawURLEncoding.EncodeToString(witness[:])
	if err != nil || got != want || got == budget || len(got) != 43 {
		test.Fatal("execution identity did not bind the compiled protocol, SQL catalog and unchanged budget", got, err)
	}
	for _, contract := range []string{
		"cxthub/process-admission/v2\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget,
		"cxthub/process-admission/v3\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget,
		"cxthub/process-admission/v4\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget,
		"cxthub/process-admission/v5\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget,
		"cxthub/process-admission/v6\x00" + releasedMigrationCatalogSHA256 + "\x00" + budget,
		"cxthub/process-admission/v7\x00" + strings.Repeat("0", 64) + "\x00" + budget,
		"cxthub/process-admission/v7\x00" + releasedMigrationCatalogSHA256 + "\x00" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, sha256.Size)),
	} {
		changed := sha256.Sum256([]byte(contract))
		if base64.RawURLEncoding.EncodeToString(changed[:]) == got {
			test.Fatal("different execution contract shares the current identity")
		}
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		if len(outbound.BoundedDocumentsControlPrefix+string(role)+"/"+got) > 63 {
			test.Fatal("execution identity exceeds the PostgreSQL session-name limit", role)
		}
	}
}

func TestProcessAdmissionFingerprintRejectsInvalidBudgetIdentity(test *testing.T) {
	for _, budget := range []string{"", "not-a-hash", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("a", 43), strings.Repeat("A", 43) + "=", strings.Repeat("é", 43)} {
		if got, err := ProcessAdmissionFingerprint(budget); got != "" || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid or noncanonical budget was accepted", budget, got, err)
		}
	}
}
