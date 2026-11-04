package domain

type DeviceLoginAuthorization struct {
	Code                    string `json:"code"`
	PollToken               string `json:"poll_token"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	VerificationURIComplete string `json:"verification_uri_complete"`
}

type DeviceLoginApproval struct {
	Status string `json:"status"`
	Token  string `json:"token"`
}
