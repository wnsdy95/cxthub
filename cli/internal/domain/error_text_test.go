package domain

import (
	"strings"
	"testing"
)

func TestSanitizeErrorTextRedactsOpaqueTokensAndQueries(t *testing.T) {
	token := strings.Repeat("Q7x_", 24) + ".Nt" // synthetic opaque run; never a real credential
	hash := "sha256:0dabdd1a29b7efed6ee54d3bcfb38fb56488eb8b0910a2b04a60cb8e22a836be"
	in := `Get "https://api.cxthub.io/api/v1/repository-connections?remote_url=https%3A%2F%2Fuser%3Asecret%40github.com%2Fa%2Fb": PUT /repos/` + hash + `: (502): challenge ` + token
	got := SanitizeErrorText(in, 0)
	for _, leaked := range []string{token, "secret", "remote_url=", "github.com"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("sanitized text still contains %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, hash) || !strings.Contains(got, "/api/v1/repository-connections?[redacted]") || !strings.Contains(got, "(502)") {
		t.Fatalf("sanitizer removed diagnostic context: %s", got)
	}
	if bounded := SanitizeErrorText(strings.Repeat("é ", 400), 101); len(bounded) > 101+len("…") || !strings.HasSuffix(bounded, "…") {
		t.Fatalf("length bound broken: %d %q", len(bounded), bounded[len(bounded)-8:])
	}
}
