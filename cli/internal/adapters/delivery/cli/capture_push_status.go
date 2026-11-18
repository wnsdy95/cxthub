package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

const capturePushMaxTries = 8

// deferredPushStatus is the read-only view of an undelivered pre-push context
// publication. Exhausted requests are never retried automatically; a manual
// push re-arms them.
type deferredPushStatus struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch"`
	BranchID  string    `json:"branch_id"`
	GitCommit string    `json:"git_commit"`
	Tries     int       `json:"tries"`
	Next      time.Time `json:"next_attempt,omitempty"`
	Exhausted bool      `json:"exhausted"`
	// Deferred counts consecutive attempts postponed by a busy or unreachable
	// server; such a request keeps retrying on its own.
	Deferred int    `json:"deferred,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (request capturePushRequest) exhausted() bool {
	if request.Tries >= capturePushMaxTries {
		return true
	}
	for _, retry := range request.MigrationRetries {
		if retry.Tries >= capturePushMaxTries {
			return true
		}
	}
	return false
}

func readCapturePushRequests(root string) (map[string]capturePushRequest, error) {
	dir := filepath.Join(root, capturePushDir)
	if err := providerfs.ValidateCxtDir(dir); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]capturePushRequest{}
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 || filepath.Ext(name) != ".json" || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := providerfs.ReadRepoFile(root, filepath.Join(capturePushDir, name))
		if err != nil {
			return nil, err
		}
		var request capturePushRequest
		if len(raw) > 65536 || json.Unmarshal(raw, &request) != nil || request.Intent.id()+".json" != name || !request.validRetries() {
			continue // The drain loop reports damaged requests; status stays read-only.
		}
		out[strings.TrimSuffix(name, ".json")] = request
	}
	return out, nil
}

func listDeferredPushes(root string) ([]deferredPushStatus, error) {
	requests, err := readCapturePushRequests(root)
	if err != nil {
		return nil, err
	}
	var out []deferredPushStatus
	for id, request := range requests {
		if request.Done || request.Retired != "" {
			continue
		}
		// Requests written by earlier binaries may hold unsanitized text.
		message := domain.SanitizeErrorText(request.Error, 240)
		out = append(out, deferredPushStatus{ID: id, Branch: request.Intent.Branch.Branch, BranchID: request.Intent.Branch.BranchID, GitCommit: request.Intent.Update.LocalOID, Tries: request.Tries, Next: request.Next, Exhausted: request.exhausted(), Deferred: request.Deferred, Error: message})
	}
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out, nil
}

// countRetiredCapturePushes reports how many undelivered requests were retired
// because their branch moved past the pushed commit.
func countRetiredCapturePushes(root string) (int, error) {
	requests, err := readCapturePushRequests(root)
	if err != nil {
		return 0, err
	}
	retired := 0
	for _, request := range requests {
		if !request.Done && request.Retired != "" {
			retired++
		}
	}
	return retired, nil
}

// rearmCapturePushes gives exhausted deferred publications a fresh retry
// budget. An empty branch re-arms every exhausted request. The drain loop
// never writes an exhausted request, so resetting it cannot race a delivery.
func rearmCapturePushes(ctx context.Context, root, branch string) (int, error) {
	requests, err := readCapturePushRequests(root)
	if err != nil {
		return 0, err
	}
	rearmed := 0
	for id, request := range requests {
		if request.Done || request.Retired != "" || !request.exhausted() || (branch != "" && request.Intent.Branch.Branch != branch) {
			continue
		}
		_, err := providerfs.WithCxtLock(ctx, root, "capture-push", id, syscall.LOCK_EX, true, func() error {
			request.Tries, request.Next, request.MigrationRetries, request.Error, request.Deferred = 0, time.Time{}, nil, "", 0
			raw, err := json.Marshal(request)
			if err != nil {
				return err
			}
			return providerfs.WriteRepoFileDurable(root, filepath.Join(capturePushDir, id+".json"), raw, 0600)
		})
		if err != nil {
			return rearmed, err
		}
		rearmed++
	}
	return rearmed, nil
}
