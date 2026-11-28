package backendclient

import (
	"net/http"
)

type reviewTransferRoundTripper func(*http.Request) (*http.Response, error)

func (f reviewTransferRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
