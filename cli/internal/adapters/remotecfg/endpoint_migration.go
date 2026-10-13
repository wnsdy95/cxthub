package remotecfg

import (
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type EndpointMigration struct {
	Version int             `json:"version"`
	From    EndpointBinding `json:"from"`
	To      EndpointBinding `json:"to"`
}

func (receipt EndpointMigration) ID() string {
	raw, _ := json.Marshal(receipt)
	return domain.HashContent(raw)
}

func (receipt EndpointMigration) Validate() error {
	if receipt.Version != 1 || receipt.To.Validate() != nil || receipt.From == receipt.To || receipt.From.RemoteURL != receipt.To.RemoteURL || receipt.From.RepoID != receipt.To.RepoID {
		return fmt.Errorf("invalid pending transport migration receipt")
	}
	if receipt.From.Version == 0 {
		endpoint, err := APIBase(receipt.From.RemoteURL)
		if err != nil || receipt.From.RepositoryID != "" || receipt.From.Endpoint != endpoint {
			return fmt.Errorf("invalid legacy pending transport source")
		}
	} else if receipt.From.Validate() != nil || receipt.From.RepositoryID != receipt.To.RepositoryID {
		return fmt.Errorf("pending transport repository ownership changed")
	}
	return nil
}

func endpointMigrations(fields map[string]json.RawMessage) (map[string]EndpointMigration, error) {
	receipts := map[string]EndpointMigration{}
	if raw, exists := fields["transport_migrations"]; exists {
		if err := json.Unmarshal(raw, &receipts); err != nil || receipts == nil || len(receipts) > 64 {
			return nil, fmt.Errorf("invalid or oversized pending transport migrations")
		}
	}
	for id, receipt := range receipts {
		if receipt.Validate() != nil || id != receipt.ID() {
			return nil, fmt.Errorf("invalid pending transport migration receipt")
		}
	}
	return receipts, nil
}

func (observed Observation) PendingEndpointSources(raw, repoID string) ([]EndpointBinding, error) {
	current, err := observed.ResolveEndpoint(raw)
	if err != nil {
		return nil, err
	}
	fields, err := observed.fields()
	if err != nil {
		return nil, err
	}
	receipts, err := endpointMigrations(fields)
	if err != nil {
		return nil, err
	}
	var sources []EndpointBinding
	for _, receipt := range receipts {
		if receipt.To == current && receipt.To.RepoID == repoID {
			sources = append(sources, receipt.From)
		}
	}
	return sources, nil
}
