package webhookdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type notificationRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip notificationRoundTripper) RoundTrip(message *http.Request) (*http.Response, error) {
	defer message.Body.Close()
	return roundTrip(message)
}

type notificationBody struct {
	closed   bool
	closeErr error
}

func (body *notificationBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (body *notificationBody) Close() error {
	body.closed = true
	return body.closeErr
}

type notificationContextKey struct{}

func notificationDelivery() outbound.NotificationDelivery {
	return outbound.NotificationDelivery{
		Destination: "https://hooks.example.test/private-credential?token=private-query",
		Job: domain.NotificationJob{
			ID: "evt_original", RepositoryID: "repository", Kind: "ref_updated", State: "pending",
			Text: "metadata <only> & \"quoted\"\n한글", Attempts: 3, Version: 7,
		},
	}
}

func notificationSender(test *testing.T, client *http.Client) *Sender {
	test.Helper()
	sender, err := NewWithClient(client)
	if err != nil {
		test.Fatal(err)
	}
	return sender
}

func prepareNotification(test *testing.T, sender *Sender, ctx context.Context, delivery outbound.NotificationDelivery) outbound.NotificationRequest {
	test.Helper()
	prepared, err := sender.Prepare(ctx, delivery)
	if err != nil {
		test.Fatal(err)
	}
	return prepared
}

func assertNotificationError(test *testing.T, actual error, reason string) *outbound.NotificationSendError {
	test.Helper()
	var failure *outbound.NotificationSendError
	if !errors.As(actual, &failure) || failure.Reason != reason {
		test.Fatalf("notification error = %v, want reason %s", actual, reason)
	}
	for _, secret := range []string{"hooks.example.test", "private-credential", "private-query", "private-user", "private-password"} {
		if strings.Contains(actual.Error(), secret) || strings.Contains(fmt.Sprintf("%+v", actual), secret) {
			test.Fatal("notification error exposed endpoint credentials")
		}
	}
	return failure
}

func TestNewSharesSecuredClient(test *testing.T) {
	test.Setenv("CXT_ALLOW_PRIVATE_WEBHOOK", "")
	first := New()
	second := New()
	if first == second || first.client != second.client || first.client != safeWebhookClient() {
		test.Fatal("senders do not share the process HTTP client")
	}
	if first.client == http.DefaultClient || first.client.Timeout != 5*time.Second || first.client.Jar != nil {
		test.Fatal("default client settings changed")
	}
	transport, valid := first.client.Transport.(*http.Transport)
	if !valid || transport == http.DefaultTransport || transport.Proxy != nil || transport.DialContext == nil || transport.DisableKeepAlives {
		test.Fatal("webhook transport must retain direct, secured, reusable connections")
	}
	if err := first.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		test.Fatal("default client permits redirects")
	}
}

func TestNewWithClientRejectsNilDependencies(test *testing.T) {
	var absentClient *http.Client
	var absentTransport *http.Transport
	var absentRoundTrip notificationRoundTripper
	var absentJar *cookiejar.Jar
	for _, candidate := range []struct {
		name   string
		client *http.Client
	}{
		{name: "nil"},
		{name: "typed-nil-client", client: absentClient},
		{name: "typed-nil-transport", client: &http.Client{Transport: absentTransport}},
		{name: "typed-nil-function", client: &http.Client{Transport: absentRoundTrip}},
		{name: "typed-nil-cookie-jar", client: &http.Client{Jar: absentJar}},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			sender, err := NewWithClient(candidate.client)
			if sender != nil || err == nil {
				test.Fatal("nil HTTP dependency accepted")
			}
		})
	}
	if sender, err := NewWithClient(&http.Client{}); err != nil || sender == nil {
		test.Fatalf("trusted client with default dependencies rejected: %v", err)
	}
}

func TestNewWithClientCopiesConfiguration(test *testing.T) {
	redirectFailure := errors.New("original redirect policy")
	jar, err := cookiejar.New(nil)
	if err != nil {
		test.Fatal(err)
	}
	transport := &http.Transport{}
	client := &http.Client{
		Transport: transport, Timeout: 2 * time.Second, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return redirectFailure },
	}
	sender := notificationSender(test, client)
	if sender.client == client || sender.client.Transport != transport || sender.client.Jar != jar || sender.client.Timeout != client.Timeout {
		test.Fatal("trusted client was not copied without replacing its dependencies")
	}
	if client.CheckRedirect(nil, nil) != redirectFailure || sender.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		test.Fatal("constructor mutated the caller redirect policy or permits redirects")
	}
	client.Timeout = time.Minute
	if sender.client.Timeout != 2*time.Second {
		test.Fatal("sender retained caller-owned client configuration")
	}
}

func TestPrepareValidatesDestinationWithoutEgress(test *testing.T) {
	var calls int
	sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected egress")
	})})
	for _, destination := range []string{
		"http://hooks.example.test/hook", "https://hooks.example.test/hook?token=private-query",
		"https://private-user:private-password@hooks.example.test/private-credential",
		"https://[2001:4860:4860::8888]:8443/hook", "http://127.0.0.1/hook",
	} {
		delivery := notificationDelivery()
		delivery.Destination = destination
		if prepared, err := sender.Prepare(context.Background(), delivery); err != nil || prepared == nil {
			test.Fatalf("valid HTTP destination rejected: %v", err)
		}
	}
	for _, destination := range []string{
		"", "hooks.example.test/hook", "/relative", "//hooks.example.test/hook",
		"ftp://hooks.example.test/hook", "file:///private-credential", "https:private-credential",
		"https:///private-credential", "https://", "http://:80/hook", "http://[::1",
		"https://hooks.example.test:invalid/private-credential", "https://hooks.example.test/%zz",
		"https://hooks.example.test/private-credential\n", "https://bad host/private-credential",
	} {
		delivery := notificationDelivery()
		delivery.Destination = destination
		prepared, err := sender.Prepare(context.Background(), delivery)
		if prepared != nil {
			test.Fatal("invalid destination produced a request")
		}
		assertNotificationError(test, err, "invalid_destination")
	}
	if prepared, err := sender.Prepare(nil, notificationDelivery()); prepared != nil || err == nil {
		test.Fatal("nil preparation context accepted")
	} else {
		assertNotificationError(test, err, "invalid_destination")
	}
	if calls != 0 {
		test.Fatal("preparation performed HTTP egress")
	}
}

func TestPreparedRequestPreservesCanonicalPayloadAndHeaders(test *testing.T) {
	delivery := notificationDelivery()
	original := delivery
	expectedBody, err := json.Marshal(map[string]string{"text": original.Job.Text})
	if err != nil {
		test.Fatal(err)
	}
	body := &notificationBody{}
	var calls int
	sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(message *http.Request) (*http.Response, error) {
		calls++
		encoded, err := io.ReadAll(message.Body)
		if err != nil {
			test.Fatal(err)
		}
		if string(encoded) != string(expectedBody) || message.ContentLength != int64(len(expectedBody)) {
			test.Fatalf("canonical payload changed: %q", encoded)
		}
		if message.Method != http.MethodPost || message.URL.String() != original.Destination {
			test.Fatal("prepared HTTP method or destination changed")
		}
		if message.Header.Get("Content-Type") != "application/json" || message.Header.Get("X-CXTHub-Event-ID") != original.Job.ID {
			test.Fatal("event identity or content type changed")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: body}, nil
	})})
	prepared := prepareNotification(test, sender, context.Background(), delivery)
	if calls != 0 || delivery != original {
		test.Fatal("preparation sent or mutated the claimed delivery")
	}
	delivery.Destination = "https://changed.example.test"
	delivery.Job.ID = "evt_changed"
	delivery.Job.Text = "changed"
	response, err := prepared.Send(context.Background())
	if err != nil || response.HTTPStatus != http.StatusNoContent || calls != 1 || !body.closed {
		test.Fatalf("prepared delivery = %+v, %v, calls %d, closed %v", response, err, calls, body.closed)
	}
}

func TestPreparedRequestUsesSendContextAndLifetime(test *testing.T) {
	preparation, cancelPreparation := context.WithCancel(context.WithValue(context.Background(), notificationContextKey{}, "prepare-actor"))
	defer cancelPreparation()
	send, cancelSend := context.WithTimeout(context.WithValue(context.Background(), notificationContextKey{}, "caller-actor"), time.Minute)
	defer cancelSend()
	expectedDeadline, _ := send.Deadline()
	var observed context.Context
	sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(message *http.Request) (*http.Response, error) {
		observed = message.Context()
		deadline, present := observed.Deadline()
		if observed.Value(notificationContextKey{}) != "caller-actor" || !present || !deadline.Equal(expectedDeadline) || observed.Err() != nil {
			test.Fatal("send did not retain caller identity, deadline and active lifetime")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: &notificationBody{}}, nil
	})})
	prepared := prepareNotification(test, sender, preparation, notificationDelivery())
	cancelPreparation()
	if _, err := prepared.Send(send); err != nil {
		test.Fatal(err)
	}
	if observed == nil || observed.Err() != nil {
		test.Fatal("sender prematurely canceled caller context")
	}
	cancelSend()
	if !errors.Is(observed.Err(), context.Canceled) {
		test.Fatal("HTTP context did not retain caller cancellation")
	}
}

func TestSendPreservesCancellationAndTransportCauses(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		cause   error
	}{
		{name: "canceled", context: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, cause: context.Canceled},
		{name: "expired", context: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, cause: context.DeadlineExceeded},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(message *http.Request) (*http.Response, error) {
				return nil, message.Context().Err()
			})})
			prepared := prepareNotification(test, sender, context.Background(), notificationDelivery())
			ctx, cancel := scenario.context()
			defer cancel()
			response, err := prepared.Send(ctx)
			assertNotificationError(test, err, "transport_failed")
			if !errors.Is(err, scenario.cause) || response != (outbound.NotificationResponse{}) {
				test.Fatalf("send lost cancellation cause: %+v %v", response, err)
			}
		})
	}
	cause := errors.New("transport failed at private-credential with private-query")
	sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})})
	prepared := prepareNotification(test, sender, context.Background(), notificationDelivery())
	response, err := prepared.Send(context.Background())
	assertNotificationError(test, err, "transport_failed")
	if !errors.Is(err, cause) || response != (outbound.NotificationResponse{}) {
		test.Fatal("transport cause or zero response changed")
	}
}

func TestSendCancellationStopsLiveRequest(test *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
		if _, err := io.Copy(io.Discard, message.Body); err != nil {
			test.Error(err)
		}
		close(started)
		<-message.Context().Done()
		close(stopped)
	}))
	defer receiver.Close()
	client := receiver.Client()
	client.Timeout = 5 * time.Second
	sender := notificationSender(test, client)
	delivery := notificationDelivery()
	delivery.Destination = receiver.URL
	prepared := prepareNotification(test, sender, context.Background(), delivery)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := prepared.Send(ctx)
		completed <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		test.Fatal("HTTP receiver did not observe the in-flight request")
	}
	cancel()
	select {
	case err := <-completed:
		assertNotificationError(test, err, "transport_failed")
		if !errors.Is(err, context.Canceled) {
			test.Fatalf("in-flight send did not preserve cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("in-flight send did not stop after cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		test.Fatal("HTTP receiver retained a canceled request")
	}
}

func TestSendReturnsHTTPStatusAndClosesBodyWithoutRetryPolicy(test *testing.T) {
	for _, status := range []int{200, 204, 299, 302, 400, 408, 425, 429, 500, 599} {
		test.Run(fmt.Sprint(status), func(test *testing.T) {
			body := &notificationBody{closeErr: errors.New("ignored close failure")}
			sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"120"}}, Body: body}, nil
			})})
			prepared := prepareNotification(test, sender, context.Background(), notificationDelivery())
			response, err := prepared.Send(context.Background())
			if err != nil || response.HTTPStatus != status || response.RetryAfter != 120*time.Second || !body.closed {
				test.Fatalf("HTTP result changed: %+v %v, closed %v", response, err, body.closed)
			}
		})
	}
}

func TestSendRetryAfterParsing(test *testing.T) {
	for _, scenario := range []struct {
		value string
		want  time.Duration
	}{
		{value: "1", want: time.Second}, {value: "120", want: 2 * time.Minute},
		{value: "86400", want: 24 * time.Hour}, {value: "86401", want: 24 * time.Hour},
		{value: "0"}, {value: "-1"}, {value: ""}, {value: "invalid"}, {value: "1.5"},
		{value: " 120 "}, {value: "9999999999999999999999999999"},
	} {
		test.Run(scenario.value, func(test *testing.T) {
			sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{scenario.value}}, Body: &notificationBody{}}, nil
			})})
			response, err := prepareNotification(test, sender, context.Background(), notificationDelivery()).Send(context.Background())
			if err != nil || response.RetryAfter != scenario.want {
				test.Fatalf("Retry-After %q = %v %v, want %v", scenario.value, response.RetryAfter, err, scenario.want)
			}
		})
	}
	for _, offset := range []time.Duration{2 * time.Minute, -2 * time.Minute, 48 * time.Hour} {
		test.Run(offset.String(), func(test *testing.T) {
			at := time.Now().UTC().Add(offset).Truncate(time.Second)
			sender := notificationSender(test, &http.Client{Transport: notificationRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": []string{at.Format(http.TimeFormat)}}, Body: &notificationBody{}}, nil
			})})
			prepared := prepareNotification(test, sender, context.Background(), notificationDelivery())
			before := time.Now()
			response, err := prepared.Send(context.Background())
			after := time.Now()
			if err != nil || response.RetryAfter < at.Sub(after) || response.RetryAfter > at.Sub(before) {
				test.Fatalf("HTTP date was clamped or misinterpreted: %+v %v", response, err)
			}
		})
	}
}

func TestSendNeverFollowsRedirectsWithInjectedClient(test *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, sameOrigin := range []bool{false, true} {
			test.Run(fmt.Sprintf("%d/same-origin-%v", status, sameOrigin), func(test *testing.T) {
				var redirected atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
					redirected.Add(1)
					writer.WriteHeader(http.StatusNoContent)
				}))
				defer target.Close()
				var origins atomic.Int64
				origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
					if message.URL.Path != "/origin" {
						redirected.Add(1)
						writer.WriteHeader(http.StatusNoContent)
						return
					}
					origins.Add(1)
					if message.Method != http.MethodPost || message.Header.Get("X-CXTHub-Event-ID") != "evt_original" {
						test.Error("redirect origin did not receive the original event")
					}
					location := target.URL + "/private-target"
					if sameOrigin {
						location = "/private-target"
					}
					writer.Header().Set("Location", location)
					writer.WriteHeader(status)
				}))
				defer origin.Close()
				var callerRedirects int
				client := origin.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error {
					callerRedirects++
					return nil
				}
				sender := notificationSender(test, client)
				delivery := notificationDelivery()
				delivery.Destination = origin.URL + "/origin"
				response, err := prepareNotification(test, sender, context.Background(), delivery).Send(context.Background())
				if err != nil || response.HTTPStatus != status || origins.Load() != 1 || redirected.Load() != 0 || callerRedirects != 0 {
					test.Fatalf("redirect leaked an event or changed the origin response: %+v %v, origin %d, redirected %d", response, err, origins.Load(), redirected.Load())
				}
				if err := client.CheckRedirect(nil, nil); err != nil || callerRedirects != 1 {
					test.Fatal("injected client redirect callback was mutated")
				}
			})
		}
	}
}

func TestSenderReusesInjectedClientAcrossDeliveries(test *testing.T) {
	var calls atomic.Int64
	var connections atomic.Int64
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	receiver.Config.ConnState = func(connection net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	receiver.StartTLS()
	defer receiver.Close()
	client := receiver.Client()
	sender := notificationSender(test, client)
	delivery := notificationDelivery()
	delivery.Destination = receiver.URL + "/hook"
	for attempt := 0; attempt < 2; attempt++ {
		response, err := prepareNotification(test, sender, context.Background(), delivery).Send(context.Background())
		if err != nil || response.HTTPStatus != http.StatusNoContent || sender.client.Transport != client.Transport {
			test.Fatalf("trusted TLS transport was replaced or delivery failed: %+v %v", response, err)
		}
	}
	if calls.Load() != 2 || connections.Load() != 1 {
		test.Fatalf("deliveries did not reuse their TLS connection: calls %d, connections %d", calls.Load(), connections.Load())
	}
}

func TestBlockedIP(test *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.5": true, "192.168.1.1": true,
		"172.16.0.1": true, "169.254.169.254": true, "fc00::1": true, "fe80::1": true,
		"0.0.0.0": true, "100.64.0.1": true, "224.0.0.1": true, "255.255.255.255": true,
		"ff02::1": true, "8.8.8.8": false, "1.1.1.1": false,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.5": true, "::ffff:100.127.255.255": true,
		"172.31.255.255": true, "172.15.255.255": false, "172.32.0.0": false,
		"100.63.255.255": false, "100.127.255.255": true, "100.128.0.0": false,
		"::": true, "2001:4860:4860::8888": false,
	}
	for address, want := range cases {
		if got := blockedIP(net.ParseIP(address)); got != want {
			test.Errorf("blockedIP(%s)=%v want %v", address, got, want)
		}
	}
	if !blockedIP(nil) {
		test.Fatal("unknown address accepted")
	}
}

func TestSafeWebhookClientBlocksLoopback(test *testing.T) {
	test.Setenv("CXT_ALLOW_PRIVATE_WEBHOOK", "")
	var received atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
		received.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	sender := New()
	defer sender.client.CloseIdleConnections()
	delivery := notificationDelivery()
	delivery.Destination = receiver.URL + "/hook"
	response, err := prepareNotification(test, sender, context.Background(), delivery).Send(context.Background())
	failure := assertNotificationError(test, err, "transport_failed")
	if failure.Cause == nil || !strings.Contains(failure.Cause.Error(), "internal network address rejected") || response != (outbound.NotificationResponse{}) {
		test.Fatal("loopback failure did not originate from SSRF rejection")
	}
	if received.Load() != 0 {
		test.Fatal("live loopback receiver received an SSRF request")
	}
}

func TestBuildWebhookClientPrivateOverrideIsExact(test *testing.T) {
	for _, value := range []string{"", "0", "true", "1"} {
		test.Run("allow-private-"+value, func(test *testing.T) {
			test.Setenv("CXT_ALLOW_PRIVATE_WEBHOOK", value)
			var received atomic.Int64
			receiver := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, message *http.Request) {
				received.Add(1)
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer receiver.Close()
			client := buildWebhookClient()
			defer client.CloseIdleConnections()
			delivery := notificationDelivery()
			delivery.Destination = receiver.URL
			response, err := prepareNotification(test, notificationSender(test, client), context.Background(), delivery).Send(context.Background())
			if value == "1" {
				if err != nil || response.HTTPStatus != http.StatusNoContent || received.Load() != 1 {
					test.Fatalf("explicit private-network override failed: %+v %v", response, err)
				}
			} else if err == nil || received.Load() != 0 {
				test.Fatal("non-explicit private-network override permitted egress")
			}
		})
	}
}

func TestWebhookDialerValidatesEveryAnswerBeforeConnecting(test *testing.T) {
	for _, addresses := range [][]string{
		{"8.8.8.8", "127.0.0.1"}, {"127.0.0.1", "8.8.8.8"},
		{"2001:4860:4860::8888", "fc00::1"}, {"8.8.8.8", "169.254.169.254"},
		{"8.8.8.8", "100.64.0.1"}, {"8.8.8.8", "::ffff:127.0.0.1"}, {"8.8.8.8", "invalid"},
	} {
		test.Run(strings.Join(addresses, "/"), func(test *testing.T) {
			ctx := context.WithValue(context.Background(), notificationContextKey{}, "caller-actor")
			var lookups, connections int
			dial := webhookDialer(false, func(actual context.Context, network, host string) ([]net.IP, error) {
				lookups++
				if actual != ctx || network != "ip" || host != "hooks.example.test" {
					test.Fatal("DNS resolution lost caller context or destination")
				}
				var resolved []net.IP
				for _, address := range addresses {
					resolved = append(resolved, net.ParseIP(address))
				}
				return resolved, nil
			}, func(context.Context, string, string) (net.Conn, error) {
				connections++
				return nil, errors.New("must not connect")
			})
			connection, err := dial(ctx, "tcp", "hooks.example.test:443")
			if connection != nil || err == nil || !strings.Contains(err.Error(), "internal network address rejected") || lookups != 1 || connections != 0 {
				test.Fatalf("mixed DNS answers were not rejected before every connection: %v, lookups %d, connections %d", err, lookups, connections)
			}
		})
	}
}

func TestWebhookDialerPinsValidatedAddressesAndRevalidatesNewConnections(test *testing.T) {
	ctx := context.WithValue(context.Background(), notificationContextKey{}, "caller-actor")
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	var lookups int
	var connected []string
	dial := webhookDialer(false, func(actual context.Context, network, host string) ([]net.IP, error) {
		lookups++
		if actual != ctx || network != "ip" || host != "hooks.example.test" {
			test.Fatal("resolver input changed")
		}
		if lookups > 1 {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("2001:4860:4860::8888"), net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}, func(actual context.Context, network, address string) (net.Conn, error) {
		if actual != ctx || network != "tcp" {
			test.Fatal("connection lost caller context or network")
		}
		connected = append(connected, address)
		if len(connected) == 1 {
			return nil, errors.New("IPv6 unavailable")
		}
		return connection, nil
	})
	actual, err := dial(ctx, "tcp", "hooks.example.test:8443")
	if err != nil || actual != connection || lookups != 1 || !reflect.DeepEqual(connected, []string{"[2001:4860:4860::8888]:8443", "8.8.8.8:8443"}) {
		test.Fatalf("validated IP fallback re-resolved or changed an address: %v, lookups %d, connected %v", err, lookups, connected)
	}
	actual, err = dial(ctx, "tcp", "hooks.example.test:8443")
	if actual != nil || err == nil || lookups != 2 || len(connected) != 2 {
		test.Fatal("new connection reused stale DNS validation after rebinding")
	}
}

func TestWebhookDialerResolutionAndConnectionFailures(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		address   string
		answers   []net.IP
		lookupErr error
		lookups   int
	}{
		{name: "malformed-address", address: "missing-port"},
		{name: "resolution-failure", address: "hooks.example.test:443", lookupErr: errors.New("DNS unavailable"), lookups: 1},
		{name: "empty-answers", address: "hooks.example.test:443", lookups: 1},
		{name: "answers-with-error", address: "hooks.example.test:443", answers: []net.IP{net.ParseIP("8.8.8.8")}, lookupErr: errors.New("incomplete DNS"), lookups: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var lookups, connections int
			dial := webhookDialer(false, func(context.Context, string, string) ([]net.IP, error) {
				lookups++
				return scenario.answers, scenario.lookupErr
			}, func(context.Context, string, string) (net.Conn, error) {
				connections++
				return nil, errors.New("must not connect")
			})
			connection, err := dial(context.Background(), "tcp", scenario.address)
			if connection != nil || err == nil || lookups != scenario.lookups || connections != 0 {
				test.Fatal("invalid or incomplete resolution reached the network")
			}
		})
	}
	lastFailure := errors.New("last validated address unavailable")
	var connections int
	dial := webhookDialer(false, func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		connections++
		if connections == 1 {
			return nil, errors.New("first validated address unavailable")
		}
		return nil, lastFailure
	})
	connection, err := dial(context.Background(), "tcp", "hooks.example.test:443")
	if connection != nil || err != lastFailure || connections != 2 {
		test.Fatal("fallback did not return the last connection failure")
	}
}
