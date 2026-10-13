package remotecfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

var ErrEndpointBindingMismatch = errors.New("transport repository binding mismatch")

type EndpointBinding struct {
	Version      int    `json:"version,omitempty"`
	RemoteURL    string `json:"remote_url"`
	RepoID       string `json:"repo_id"`
	RepositoryID string `json:"repository_id,omitempty"`
	Endpoint     string `json:"endpoint"`
}

func CanonicalEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("endpoint must be an absolute HTTPS (or loopback HTTP) API URL without credentials, query or fragment")
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	address := net.ParseIP(parsed.Hostname())
	loopback := parsed.Hostname() == "localhost" || address != nil && address.IsLoopback()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", fmt.Errorf("endpoint requires HTTPS except on loopback")
	}
	if strings.ContainsAny(parsed.Host, "\\%") || strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("invalid endpoint host")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("invalid endpoint port")
		}
	}
	if parsed.RawPath != "" || strings.TrimSuffix(parsed.Path, "/") != "/api/v1" {
		return "", fmt.Errorf("endpoint path must be /api/v1")
	}
	parsed.Path = "/api/v1"
	return parsed.String(), nil
}

func (binding EndpointBinding) Validate() error {
	canonical, err := CanonicalURL(binding.RemoteURL)
	endpoint, endpointErr := CanonicalEndpoint(binding.Endpoint)
	if err != nil || canonical != binding.RemoteURL || endpointErr != nil || endpoint != binding.Endpoint || binding.Version != 1 || binding.RepoID != RepoIDFor(canonical) || !validRepositorySegment(binding.RepositoryID) {
		return fmt.Errorf("invalid transport endpoint binding")
	}
	return nil
}

func endpointBindings(fields map[string]json.RawMessage) (map[string]EndpointBinding, error) {
	bindings := map[string]EndpointBinding{}
	if raw, exists := fields["transport_endpoints"]; exists {
		if err := json.Unmarshal(raw, &bindings); err != nil || bindings == nil {
			return nil, fmt.Errorf("invalid transport endpoints")
		}
	}
	for identity, binding := range bindings {
		if identity != binding.RemoteURL || binding.Validate() != nil {
			return nil, fmt.Errorf("invalid transport endpoint binding")
		}
	}
	return bindings, nil
}

func (observed Observation) ResolveEndpoint(raw string) (EndpointBinding, error) {
	canonical, err := CanonicalURL(raw)
	if err != nil {
		return EndpointBinding{}, err
	}
	fields, err := observed.fields()
	if err != nil {
		return EndpointBinding{}, err
	}
	bindings, err := endpointBindings(fields)
	if err != nil {
		return EndpointBinding{}, err
	}
	if binding, exists := bindings[canonical]; exists {
		return binding, nil
	}
	endpoint, err := APIBase(canonical)
	return EndpointBinding{RemoteURL: canonical, RepoID: RepoIDFor(canonical), Endpoint: endpoint}, err
}

func ResolveEndpoint(ctx context.Context, root, raw string) (EndpointBinding, error) {
	observed, err := Observe(ctx, root)
	if err != nil {
		return EndpointBinding{}, err
	}
	return observed.ResolveEndpoint(raw)
}

func (observed Observation) OriginEndpoint(fallback string) (EndpointBinding, error) {
	remotes, err := observed.Remotes()
	if err != nil {
		return EndpointBinding{}, err
	}
	if origin := remotes["origin"]; origin != "" {
		return observed.ResolveEndpoint(origin)
	}
	return EndpointBinding{Endpoint: fallback}, nil
}

func (observed Observation) CaptureIdentity() (domain.DocumentIdentity, error) {
	return captureIdentityFromObservation(observed, nil)
}

func SetEndpoint(ctx context.Context, expected Observation, name string, binding EndpointBinding, rootConfirmed bool) error {
	return SetEndpointWithPending(ctx, expected, name, binding, rootConfirmed, false)
}

func SetEndpointWithPending(ctx context.Context, expected Observation, name string, binding EndpointBinding, rootConfirmed, migratePending bool) error {
	if !validRemoteName(name) || binding.Validate() != nil {
		return fmt.Errorf("invalid remote endpoint binding")
	}
	remotes, err := expected.Remotes()
	if err != nil {
		return err
	}
	if remotes[name] != binding.RemoteURL {
		return ErrChanged
	}
	previous, err := expected.ResolveEndpoint(binding.RemoteURL)
	if err != nil {
		return err
	}
	if previous.RepositoryID != "" && previous.RepositoryID != binding.RepositoryID {
		return fmt.Errorf("transport migration changed repository ownership identity")
	}
	identity, err := expected.CaptureIdentity()
	if err != nil {
		return err
	}
	keepRoot := remotes["origin"] == binding.RemoteURL && identity == domain.DocumentIdentityRootV1
	if keepRoot && !rootConfirmed {
		return fmt.Errorf("fresh root capture capability confirmation required; config unchanged")
	}
	return mutate(ctx, expected.root, &expected, false, func(fields map[string]json.RawMessage, _ Observation) error {
		receipts, err := endpointMigrations(fields)
		if err != nil {
			return err
		}
		if migratePending {
			if previous == binding {
				found := false
				for _, receipt := range receipts {
					found = found || receipt.To == binding
				}
				if !found {
					return fmt.Errorf("no prior transport transition to authorize; pending work retained")
				}
			} else {
				receipt := EndpointMigration{Version: 1, From: previous, To: binding}
				if err := receipt.Validate(); err != nil {
					return err
				}
				receipts[receipt.ID()] = receipt
				if len(receipts) > 64 {
					return fmt.Errorf("pending transport migration receipt limit reached")
				}
				fields["transport_migrations"], _ = json.Marshal(receipts)
			}
		}
		bindings, err := endpointBindings(fields)
		if err != nil {
			return err
		}
		bindings[binding.RemoteURL] = binding
		fields["transport_endpoints"], _ = json.Marshal(bindings)
		if keepRoot {
			capture, err := captureBinding(binding.RemoteURL)
			if err != nil {
				return err
			}
			capture.Endpoint = binding.Endpoint
			fields["capture_document"], _ = json.Marshal(capture)
		}
		return nil
	})
}
