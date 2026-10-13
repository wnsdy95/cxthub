package mcp

import "testing"

func TestMCPPublicOrigins(t *testing.T) {
	for _, origin := range []string{"https://cxthub.io", "https://mcp.cxthub.io/", "http://localhost:8908", "http://[::1]:8908"} {
		if _, err := canonicalMCPOrigin(origin); err != nil {
			t.Fatalf("valid origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"", "//mcp.cxthub.io", "http://mcp.cxthub.io", "https://user:secret@mcp.cxthub.io", "https://mcp.cxthub.io/mcp", "https://mcp.cxthub.io/?", "https://mcp.cxthub.io/#fragment", "https://mcp.cxthub.io/?resource=evil"} {
		if _, err := canonicalMCPOrigin(origin); err == nil {
			t.Fatalf("invalid resource origin accepted: %q", origin)
		}
	}
}
