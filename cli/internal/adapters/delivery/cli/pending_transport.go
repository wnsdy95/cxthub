package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

var errPendingTransportChanged = errors.New("pending transport lacks an exact current migration authorization; queued work retained")

func pendingTransportMismatch(err error) bool {
	return errors.Is(err, errPendingTransportChanged) || errors.Is(err, remotecfg.ErrEndpointBindingMismatch)
}

func captureDestination(binding remotecfg.EndpointBinding) CapturePushDestination {
	destination := CapturePushDestination{URL: binding.RemoteURL}
	if binding.Version != 0 {
		destination.Endpoint, destination.RepositoryID = binding.Endpoint, binding.RepositoryID
	}
	return destination
}

func pendingTransportDestinations(root, repoID string) (CapturePushDestination, []CapturePushDestination, error) {
	var current CapturePushDestination
	observed, err := remotecfg.Observe(context.Background(), root)
	if err != nil {
		return current, nil, err
	}
	remotes, err := observed.Remotes()
	if err != nil {
		return current, nil, err
	}
	if remotes["origin"] == "" {
		current.URL, current.APIOnly = os.Getenv("CXT_REMOTE"), true
		if current.URL == "" {
			return current, nil, fmt.Errorf("context origin unavailable")
		}
		return current, nil, nil
	}
	binding, err := observed.ResolveEndpoint(remotes["origin"])
	if err != nil {
		return current, nil, err
	}
	current = captureDestination(binding)
	if repoID == "" {
		return current, nil, nil
	}
	sources, err := observed.PendingEndpointSources(binding.RemoteURL, repoID)
	if err != nil {
		return current, nil, err
	}
	var prior []CapturePushDestination
	for _, source := range sources {
		prior = append(prior, captureDestination(source))
	}
	return current, prior, nil
}

func resolvePendingTransport(root, repoID string, match func(CapturePushDestination) bool) (CapturePushDestination, error) {
	destination, _, err := resolvePendingTransportRoute(root, repoID, match)
	return destination, err
}

func resolvePendingTransportRoute(root, repoID string, match func(CapturePushDestination) bool) (CapturePushDestination, string, error) {
	current, prior, err := pendingTransportDestinations(root, repoID)
	if err != nil {
		return current, "", err
	}
	if match(current) {
		return current, "", nil
	}
	for _, source := range prior {
		if match(source) {
			raw, _ := json.Marshal([]any{repoID, source, current})
			return current, domain.HashContent(raw), nil
		}
	}
	return CapturePushDestination{}, "", errPendingTransportChanged
}

func mergeScopeMatcher(root string, current mergeEvidence) (func(mergeEvidence) bool, error) {
	destination, prior, err := pendingTransportDestinations(root, current.RepoID)
	if err != nil {
		return nil, err
	}
	if mergeDestinationHash(destination) != current.Remote {
		return nil, errPendingTransportChanged
	}
	return func(saved mergeEvidence) bool {
		comparable := current
		comparable.Remote = saved.Remote
		if !sameMergeScope(saved, comparable) {
			return false
		}
		if saved.Remote == current.Remote {
			return true
		}
		for _, source := range prior {
			if saved.Remote == mergeDestinationHash(source) {
				return true
			}
		}
		return false
	}, nil
}
