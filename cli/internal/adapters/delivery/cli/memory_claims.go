package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Authoring is explicit: automatic distillation never invents code/file scopes.
func readMemoryClaims(path string) ([]domain.MemoryClaim, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxBytes = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("memory claims file exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var claims []domain.MemoryClaim
	if err := decoder.Decode(&claims); err != nil {
		return nil, fmt.Errorf("memory claims: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("memory claims must contain exactly one JSON array")
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("memory claims array is empty")
	}
	digest := domain.MemoryDigest{ClaimsVersion: domain.MemoryClaimsVersion,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: domain.HashContent([]byte("input-validation")), Claims: claims}}}
	if err := digest.ValidateMemoryClaims(); err != nil {
		return nil, err
	}
	return claims, nil
}
