package webhookdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Sender struct {
	client *http.Client
}

type request struct {
	client  *http.Client
	message *http.Request
}

var _ outbound.NotificationSender = (*Sender)(nil)
var _ outbound.NotificationRequest = (*request)(nil)

func New() *Sender {
	return &Sender{client: safeWebhookClient()}
}

func NewWithClient(client *http.Client) (*Sender, error) {
	if client == nil || typedNil(client.Transport) || typedNil(client.Jar) {
		return nil, errors.New("webhook: HTTP client required without typed-nil dependencies")
	}
	configured := *client
	configured.CheckRedirect = rejectWebhookRedirect
	return &Sender{client: &configured}, nil
}

func typedNil(value any) bool {
	if value == nil {
		return false
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (sender *Sender) Prepare(ctx context.Context, delivery outbound.NotificationDelivery) (outbound.NotificationRequest, error) {
	if !webhookSchemeOK(delivery.Destination) {
		return nil, &outbound.NotificationSendError{Reason: "invalid_destination"}
	}
	body, err := json.Marshal(map[string]string{"text": delivery.Job.Text})
	if err != nil {
		return nil, &outbound.NotificationSendError{Reason: "encoding_failed", Cause: err}
	}
	message, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.Destination, bytes.NewReader(body))
	if err != nil {
		return nil, &outbound.NotificationSendError{Reason: "invalid_destination", Cause: err}
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("X-CXTHub-Event-ID", delivery.Job.ID)
	return &request{client: sender.client, message: message}, nil
}

func (prepared *request) Send(ctx context.Context) (outbound.NotificationResponse, error) {
	response, err := prepared.client.Do(prepared.message.Clone(ctx))
	if err != nil {
		return outbound.NotificationResponse{}, &outbound.NotificationSendError{Reason: "transport_failed", Cause: err}
	}
	response.Body.Close()
	return outbound.NotificationResponse{
		HTTPStatus: response.StatusCode,
		RetryAfter: retryAfter(response.Header.Get("Retry-After")),
	}, nil
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return time.Until(at)
	}
	return 0
}

func webhookSchemeOK(raw string) bool {
	parsed, err := neturl.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != ""
}

func blockedIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

var (
	webhookClientOnce sync.Once
	webhookClient     *http.Client
)

func safeWebhookClient() *http.Client {
	webhookClientOnce.Do(func() { webhookClient = buildWebhookClient() })
	return webhookClient
}

func buildWebhookClient() *http.Client {
	allowPrivate := os.Getenv("CXT_ALLOW_PRIVATE_WEBHOOK") == "1"
	base := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DialContext: webhookDialer(allowPrivate, net.DefaultResolver.LookupIP, base.DialContext)},
		CheckRedirect: rejectWebhookRedirect,
	}
}

func rejectWebhookRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func webhookDialer(allowPrivate bool, lookupIP func(context.Context, string, string) ([]net.IP, error), connect func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := lookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("webhook: host resolution failed")
		}
		var lastErr error
		for _, ip := range ips {
			if !allowPrivate && blockedIP(ip) {
				return nil, fmt.Errorf("webhook: internal network address rejected (%s)", ip)
			}
		}
		for _, ip := range ips {
			connection, dialErr := connect(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}
