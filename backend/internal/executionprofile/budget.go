package executionprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	ManifestVersion  = 1
	MaxManifestBytes = 64 << 10
)

type Budget struct {
	Version             int          `json:"version"`
	Profile             string       `json:"profile"`
	Roles               []RoleBudget `json:"roles"`
	BaselineActive      int32        `json:"baseline_active"`
	BaselineBulk        int32        `json:"baseline_bulk"`
	BaselineConnections int32        `json:"baseline_connections"`
	ReservedConnections int32        `json:"reserved_connections"`
	BaselineCPU         int64        `json:"baseline_cpu"`
	BaselineMemoryBytes int64        `json:"baseline_memory_bytes"`
	BaselineSpoolBytes  int64        `json:"baseline_spool_bytes"`
}

type RoleBudget struct {
	Role                outbound.ProcessRole `json:"role"`
	Instances           int32                `json:"instances"`
	Active              int32                `json:"active"`
	Bulk                int32                `json:"bulk"`
	DataPoolMax         int32                `json:"data_pool_max"`
	CoordinationPoolMax int32                `json:"coordination_pool_max"`
	RenewalReserve      int32                `json:"renewal_reserve"`
	Queue               int32                `json:"queue"`
	CPU                 int64                `json:"cpu"`
	MemoryBytes         int64                `json:"memory_bytes"`
	SpoolBytes          int64                `json:"spool_bytes"`
}

func Parse(raw []byte) (Budget, error) {
	if len(raw) == 0 || len(raw) > MaxManifestBytes {
		return Budget{}, invalidBudget("manifest must contain at most 64 KiB of JSON")
	}
	if !utf8.Valid(raw) {
		return Budget{}, invalidBudget("manifest must contain valid UTF-8 JSON")
	}
	fields, err := decodeBudgetObject(raw, []string{
		"version", "profile", "roles", "baseline_active", "baseline_bulk",
		"baseline_connections", "reserved_connections", "baseline_cpu",
		"baseline_memory_bytes", "baseline_spool_bytes",
	})
	if err != nil {
		return Budget{}, err
	}
	var roles []json.RawMessage
	if err := json.Unmarshal(fields["roles"], &roles); err != nil || len(roles) != 3 {
		return Budget{}, invalidBudget("manifest must declare exactly three roles")
	}
	for _, rawRole := range roles {
		if _, err := decodeBudgetObject(rawRole, []string{
			"role", "instances", "active", "bulk", "data_pool_max",
			"coordination_pool_max", "renewal_reserve", "queue", "cpu",
			"memory_bytes", "spool_bytes",
		}); err != nil {
			return Budget{}, err
		}
	}
	var budget Budget
	if err := json.Unmarshal(raw, &budget); err != nil {
		return Budget{}, invalidBudget("manifest fields must have the declared string and integer types")
	}
	if err := budget.Validate(); err != nil {
		return Budget{}, err
	}
	return budget, nil
}

func (budget Budget) Validate() error {
	if budget.Version == 0 || budget.Profile == "" {
		return invalidBudget("manifest version and profile must be explicit")
	}
	if budget.Version != ManifestVersion || (budget.Profile != outbound.BoundedDocumentsProfile && budget.Profile != outbound.BoundedWorkersProfile) {
		return fmt.Errorf("%w: execution profile version or profile is not supported", errors.ErrUnsupported)
	}
	if budget.BaselineActive != 4 || budget.BaselineBulk != 2 {
		return invalidBudget("bounded profile requires baseline active four and shared bulk two")
	}
	if budget.BaselineConnections <= 0 || budget.ReservedConnections <= 0 ||
		budget.BaselineCPU <= 0 || budget.BaselineMemoryBytes <= 0 || budget.BaselineSpoolBytes <= 0 {
		return invalidBudget("baseline and operator reserve budgets must be explicit and positive")
	}
	if len(budget.Roles) != 3 {
		return invalidBudget("manifest must declare exactly three roles")
	}
	seen := make(map[outbound.ProcessRole]bool, 3)
	connections := int64(budget.ReservedConnections)
	var queued int64
	remainingCPU := budget.BaselineCPU
	remainingMemory := budget.BaselineMemoryBytes
	remainingSpool := budget.BaselineSpoolBytes
	for _, role := range budget.Roles {
		var active int32
		switch role.Role {
		case outbound.ProcessAPI:
			active = 2
		case outbound.ProcessMCP, outbound.ProcessWorker:
			active = 1
		default:
			return invalidBudget("manifest contains an unrecognized process role")
		}
		if seen[role.Role] {
			return invalidBudget("manifest must declare each process role once")
		}
		seen[role.Role] = true
		if role.Instances != 1 || role.Active != active || role.Bulk != 1 {
			return invalidBudget("role instances, active and bulk limits must match the bounded profile")
		}
		if role.CoordinationPoolMax != 2 || role.RenewalReserve <= 0 ||
			int64(role.DataPoolMax) < int64(role.Active)+int64(role.RenewalReserve) {
			return invalidBudget("role pools must include two coordination sessions and an explicit data renewal reserve")
		}
		if role.Queue <= 0 || role.CPU <= 0 || role.MemoryBytes <= 0 || role.SpoolBytes <= 0 {
			return invalidBudget("role queue, CPU, soft memory and spool envelopes must be explicit and positive")
		}
		if role.CPU > remainingCPU || role.MemoryBytes > remainingMemory || role.SpoolBytes > remainingSpool {
			return invalidBudget("declared role envelopes exceed the explicit baseline budgets")
		}
		remainingCPU -= role.CPU
		remainingMemory -= role.MemoryBytes
		remainingSpool -= role.SpoolBytes
		connections += int64(role.DataPoolMax) + int64(role.CoordinationPoolMax)
		queued += int64(role.Queue)
	}
	if connections > int64(budget.BaselineConnections) {
		return invalidBudget("role pools and operator reserve exceed the explicit connection baseline")
	}
	if queued > 16 {
		return invalidBudget("aggregate role queue limit must not exceed sixteen")
	}
	return nil
}

func (budget Budget) ForRole(role outbound.ProcessRole) (RoleBudget, error) {
	if err := budget.Validate(); err != nil {
		return RoleBudget{}, err
	}
	for _, candidate := range budget.Roles {
		if candidate.Role == role {
			return candidate, nil
		}
	}
	return RoleBudget{}, invalidBudget("process role is not declared by the manifest")
}

func (budget Budget) AdmissionConfig(role outbound.ProcessRole) (outbound.ProcessAdmissionConfig, error) {
	roleBudget, err := budget.ForRole(role)
	if err != nil {
		return outbound.ProcessAdmissionConfig{}, err
	}
	fingerprint, err := budget.Fingerprint()
	if err != nil {
		return outbound.ProcessAdmissionConfig{}, err
	}
	return outbound.ProcessAdmissionConfig{
		Role:                roleBudget.Role,
		DataPoolMax:         roleBudget.DataPoolMax,
		CoordinationPoolMax: roleBudget.CoordinationPoolMax,
		DBConnectionBudget:  budget.BaselineConnections,
		ReservedConnections: budget.ReservedConnections,
		BudgetFingerprint:   fingerprint,
	}, nil
}

func (budget Budget) Fingerprint() (string, error) {
	if err := budget.Validate(); err != nil {
		return "", err
	}
	budget.Roles = append([]RoleBudget(nil), budget.Roles...)
	sort.Slice(budget.Roles, func(left, right int) bool {
		return budget.Roles[left].Role < budget.Roles[right].Role
	})
	canonical, err := json.Marshal(budget)
	if err != nil {
		return "", invalidBudget("execution budget cannot be encoded")
	}
	digest := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func decodeBudgetObject(raw []byte, required []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, invalidBudget("manifest and role entries must be JSON objects")
	}
	fields := make(map[string]json.RawMessage, len(required))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, invalidBudget("manifest contains invalid JSON")
		}
		name, ok := token.(string)
		if !ok {
			return nil, invalidBudget("manifest contains an invalid field name")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, invalidBudget("manifest contains a duplicate field")
		}
		recognized := false
		for _, expected := range required {
			if name == expected {
				recognized = true
				break
			}
		}
		if !recognized {
			return nil, invalidBudget("manifest contains an unrecognized field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalidBudget("manifest contains invalid JSON")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, invalidBudget("manifest fields must not be null")
		}
		fields[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, invalidBudget("manifest contains invalid JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, invalidBudget("manifest must contain exactly one JSON value")
	}
	if len(fields) != len(required) {
		return nil, invalidBudget("manifest must explicitly declare every required field")
	}
	return fields, nil
}

func invalidBudget(message string) error {
	return fmt.Errorf("%w: %s", domain.ErrValidation, message)
}
