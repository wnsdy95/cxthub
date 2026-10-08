package remotecfg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// This is a local preference bound to the destination confirmed by an explicit
// command. It is not a publication receipt or a current authorization cache.
type captureDocumentBinding struct {
	Version   int                     `json:"version"`
	Identity  domain.DocumentIdentity `json:"identity"`
	RepoID    string                  `json:"repo_id"`
	RemoteURL string                  `json:"remote_url"`
	Endpoint  string                  `json:"endpoint"`
}

func captureBinding(origin string) (captureDocumentBinding, error) {
	canonical, err := CanonicalURL(origin)
	if err != nil {
		return captureDocumentBinding{}, err
	}
	endpoint, err := APIBase(canonical)
	if err != nil {
		return captureDocumentBinding{}, err
	}
	return captureDocumentBinding{Version: 1, Identity: domain.DocumentIdentityRootV1, RepoID: RepoIDFor(canonical), RemoteURL: canonical, Endpoint: endpoint}, nil
}

// SetCaptureDocumentIdentity must be called only after fresh capability
// confirmation of expected's origin. Confirmation is deliberately outside this
// CAS/capture lock. Clearing a preference is always local and preserves objects.
func SetCaptureDocumentIdentity(ctx context.Context, expected Observation, identity domain.DocumentIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	return mutate(ctx, expected.root, &expected, false, func(fields map[string]json.RawMessage, _ Observation) error {
		if identity == domain.DocumentIdentityLegacy {
			delete(fields, "capture_document")
			return nil
		}
		remotes, err := decodeRemotes(fields)
		if err != nil {
			return err
		}
		binding, err := captureBinding(remotes["origin"])
		if err != nil {
			return fmt.Errorf("capture.identity requires a configured origin: %w", err)
		}
		fields["capture_document"], err = json.Marshal(binding)
		return err
	})
}

func captureIdentityFromObservation(observation Observation, selected *domain.Repo) (domain.DocumentIdentity, error) {
	fields, err := observation.fields()
	if err != nil {
		return "", err
	}
	raw, exists := fields["capture_document"]
	if !exists {
		return domain.DocumentIdentityLegacy, nil
	}
	var binding captureDocumentBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return "", err
	}
	expected, err := captureBinding(binding.RemoteURL)
	if err != nil || binding != expected {
		return "", fmt.Errorf("invalid capture_document preference: %w", domain.ErrUnsupportedDocumentIdentity)
	}
	remotes, err := decodeRemotes(fields)
	if err != nil {
		return "", err
	}
	if remotes["origin"] != binding.RemoteURL {
		return domain.DocumentIdentityLegacy, nil
	}
	if selected != nil && (selected.ID != binding.RepoID || selected.RemoteURL != binding.RemoteURL) {
		return domain.DocumentIdentityLegacy, nil
	}
	return binding.Identity, nil
}

// CaptureIdentity is the passive display of the preference for current origin.
func CaptureIdentity(ctx context.Context, root string) (domain.DocumentIdentity, error) {
	observation, err := Observe(ctx, root)
	if err != nil {
		return "", err
	}
	identity, err := captureIdentityFromObservation(observation, nil)
	if err != nil {
		return "", err
	}
	return identity, ctx.Err()
}
func captureIdentity(ctx context.Context, root string, repo domain.Repo) (domain.DocumentIdentity, error) {
	observation, err := Observe(ctx, root)
	if err != nil {
		return "", err
	}
	identity, err := captureIdentityFromObservation(observation, &repo)
	if err != nil {
		return "", err
	}
	return identity, ctx.Err()
}
func (g *GitContextWithRemote) CaptureDocumentIdentity(ctx context.Context, repo domain.Repo) (domain.DocumentIdentity, error) {
	return captureIdentity(ctx, g.repoRoot, repo)
}

var _ outbound.CaptureDocumentPolicy = (*GitContextWithRemote)(nil)
