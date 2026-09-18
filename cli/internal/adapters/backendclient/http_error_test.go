package backendclient

import (
	"strings"
	"testing"
)

func TestHTTPErrorPreservesEndpointWithoutQueryCredentials(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"not_found","message":"not found"}}`, `not found`} {
		err := newHTTPError(404, "GET", "https://private-user:private-password@example.test/repos/repo/memories?token=private-token#private-fragment", []byte(body))
		got := err.Error()
		if !strings.Contains(got, "GET /repos/repo/memories") || !strings.Contains(got, "404") {
			t.Fatal(got)
		}
		for _, private := range []string{"private-user", "private-password", "private-token", "private-fragment", "example.test"} {
			if strings.Contains(got, private) {
				t.Fatal("URL credentials/query leaked", got)
			}
		}
		if err.StatusCode() != 404 {
			t.Fatal("status classification changed")
		}
	}
}
