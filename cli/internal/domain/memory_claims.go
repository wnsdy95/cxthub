package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

const MemoryClaimsVersion uint32 = 1

// MemoryClaim is an author's explicitly scoped statement. File evidence can
// assess its declared scope; it cannot prove the prose's semantic truth.
type MemoryClaim struct {
	Kind string           `json:"kind"` // code, decision, rationale
	Text string           `json:"text"`
	Code *MemoryCodeScope `json:"code,omitempty"`
}

type MemoryCodeScope struct {
	Commit string   `json:"commit"`
	Parent string   `json:"parent,omitempty"`
	Paths  []string `json:"paths"`
}

func (d MemoryDigest) HasMemoryClaims() bool {
	for _, f := range d.Fragments {
		if len(f.Claims) > 0 {
			return true
		}
	}
	return false
}

// ValidateMemoryClaims checks the wire declaration only. Publication bindings,
// ancestry and current applicability belong to the repository application query.
func (d MemoryDigest) ValidateMemoryClaims() error {
	if d.ClaimsVersion != 0 && d.ClaimsVersion != MemoryClaimsVersion {
		return fmt.Errorf("unsupported memory claims version")
	}
	total := 0
	for _, f := range d.Fragments {
		total += len(f.Claims)
		if len(f.Claims) > 256 || total > 2048 {
			return fmt.Errorf("too many memory claims")
		}
		if len(f.Claims) > 0 {
			if err := ValidateContentHash(f.SourceSnapshot); err != nil {
				return fmt.Errorf("memory claim source: %w", err)
			}
		}
		for _, c := range f.Claims {
			if d.ClaimsVersion != MemoryClaimsVersion {
				return fmt.Errorf("memory claims require claims_version=1")
			}
			if strings.TrimSpace(c.Text) == "" || len(c.Text) > 8192 {
				return fmt.Errorf("invalid memory claim text")
			}
			switch c.Kind {
			case "decision", "rationale":
				if c.Code != nil {
					return fmt.Errorf("historical memory claims cannot declare code scope")
				}
			case "code":
				if c.Code == nil || !memoryClaimOID(c.Code.Commit) || (c.Code.Parent != "" && (!memoryClaimOID(c.Code.Parent) || len(c.Code.Parent) != len(c.Code.Commit))) {
					return fmt.Errorf("code claim requires full source Git SHA")
				}
				if len(c.Code.Paths) == 0 || len(c.Code.Paths) > 20 {
					return fmt.Errorf("code claim requires 1 to 20 exact file paths")
				}
				seen := map[string]bool{}
				for _, p := range c.Code.Paths {
					if p == "" || len(p) > 4096 || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || seen[p] {
						return fmt.Errorf("invalid or duplicate code claim path")
					}
					for _, part := range strings.Split(p, "/") {
						if part == "" || part == "." || part == ".." {
							return fmt.Errorf("invalid code claim path")
						}
					}
					seen[p] = true
				}
			default:
				return fmt.Errorf("unknown memory claim kind")
			}
		}
	}
	return nil
}
func memoryClaimOID(s string) bool {
	if (len(s) != 40 && len(s) != 64) || strings.ToLower(s) != s || strings.Trim(s, "0") == "" {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
