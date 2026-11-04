package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type DeviceLoginClient struct {
	base string
	http *http.Client
}

var _ outbound.DeviceLoginClient = (*DeviceLoginClient)(nil)

func NewDeviceLoginClient(base string) *DeviceLoginClient {
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DeviceLoginClient{base: base, http: &client}
}

func (client *DeviceLoginClient) Start(ctx context.Context, label string) (domain.DeviceLoginAuthorization, error) {
	var result domain.DeviceLoginAuthorization
	err := client.post(ctx, "/auth/device/start", map[string]string{"label": label}, &result)
	return result, err
}

func (client *DeviceLoginClient) Poll(ctx context.Context, code, secret string) (domain.DeviceLoginApproval, error) {
	var result domain.DeviceLoginApproval
	err := client.post(ctx, "/auth/device/poll", map[string]string{"code": code, "poll_token": secret}, &result)
	return result, err
}

func (client *DeviceLoginClient) VerifyToken(ctx context.Context, token string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.base+"/me", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.http.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token verification failed (%s) — run cxt login again", response.Status)
	}
	var identity struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(response.Body).Decode(&identity)
	return identity.Username, nil
}

func (client *DeviceLoginClient) post(ctx context.Context, path string, body, output any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.base+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s", response.Status)
	}
	return json.NewDecoder(response.Body).Decode(output)
}
