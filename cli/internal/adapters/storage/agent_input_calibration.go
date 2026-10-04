package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const agentInputCalibrationNamespace = "agent-input-calibrations"
const maxAgentInputCalibrationBytes = 4096

// Only a runtime scope hash and numeric feedback are persisted. The checksum
// detects damaged but still valid JSON; it is not an authenticity assertion.
type agentInputCalibrationRecord struct {
	Version int `json:"version"`
	domain.AgentInputCalibration
	Checksum domain.ContentHash `json:"checksum"`
}

func agentInputCalibrationRelative(scope domain.ContentHash) string {
	return filepath.Join(".cxt", agentInputCalibrationNamespace, hexOf(scope)+".json")
}

func (s *FileStore) ReadAgentInputCalibration(ctx context.Context, scope domain.ContentHash) (domain.AgentInputCalibration, error) {
	zero := domain.AgentInputCalibration{Scope: scope}
	if err := ctx.Err(); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	if err := zero.Validate(scope); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	relative := agentInputCalibrationRelative(scope)
	if err := validateCxtWritePath(filepath.Join(s.repoRoot, relative)); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	// Root confines the open even if a parent changes after path validation.
	root, err := os.OpenRoot(s.repoRoot)
	if err != nil {
		return domain.AgentInputCalibration{}, err
	}
	defer root.Close()
	f, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return zero, nil
	}
	if err != nil {
		return domain.AgentInputCalibration{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return domain.AgentInputCalibration{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxAgentInputCalibrationBytes {
		return domain.AgentInputCalibration{}, domain.ErrHashMismatch
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxAgentInputCalibrationBytes+1))
	if err != nil {
		return domain.AgentInputCalibration{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	return decodeAgentInputCalibration(raw, scope)
}

func decodeAgentInputCalibration(raw []byte, scope domain.ContentHash) (domain.AgentInputCalibration, error) {
	invalid := func() (domain.AgentInputCalibration, error) {
		return domain.AgentInputCalibration{}, domain.ErrHashMismatch
	}
	if len(raw) > maxAgentInputCalibrationBytes {
		return invalid()
	}
	// encoding/json alone accepts duplicate keys, case aliases and null integer
	// fields. All fields are mandatory here: damage must not reset a bound to zero.
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := make(map[string]bool, 5)
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return invalid()
		}
		switch key {
		case "version", "scope", "overhead_tokens", "input_ceiling_tokens", "checksum":
		default:
			return invalid()
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid()
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') || len(seen) != 5 {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	var record agentInputCalibrationRecord
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.AgentInputCalibration.Validate(scope) != nil {
		return invalid()
	}
	canonical, err := json.Marshal(record.AgentInputCalibration)
	if err != nil || record.Checksum != domain.HashContent(canonical) {
		return invalid()
	}
	return record.AgentInputCalibration, nil
}

func (s *FileStore) MergeAgentInputCalibration(ctx context.Context, observation domain.AgentInputCalibration) (domain.AgentInputCalibration, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	if err := observation.Validate(observation.Scope); err != nil {
		return domain.AgentInputCalibration{}, err
	}
	var merged domain.AgentInputCalibration
	err := s.withMutationLock(ctx, agentInputCalibrationNamespace, hexOf(observation.Scope), func() error {
		current, err := s.ReadAgentInputCalibration(ctx, observation.Scope)
		if err != nil {
			return err
		}
		merged, err = current.Merge(observation)
		if err != nil {
			return err
		}
		canonical, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(agentInputCalibrationRecord{Version: 1, AgentInputCalibration: merged, Checksum: domain.HashContent(canonical)})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return providerfs.WriteRepoFileDurable(s.repoRoot, agentInputCalibrationRelative(observation.Scope), raw, 0600)
	})
	if err != nil {
		return domain.AgentInputCalibration{}, err
	}
	return merged, nil
}

var _ outbound.AgentInputCalibrationStore = (*FileStore)(nil)
