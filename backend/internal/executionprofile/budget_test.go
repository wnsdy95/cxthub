package executionprofile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestBudgetParseAndRoleAdmissionConfig(test *testing.T) {
	want := localBudgetFixture()
	budget, err := executionprofile.Parse(budgetJSON(test, want))
	if err != nil || !reflect.DeepEqual(budget, want) {
		test.Fatalf("parsed budget = %#v, %v; want %#v", budget, err, want)
	}
	var active, bulk int32
	fingerprint, err := want.Fingerprint()
	if err != nil {
		test.Fatal(err)
	}
	for _, expected := range want.Roles {
		actual, err := budget.ForRole(expected.Role)
		if err != nil || actual != expected {
			test.Fatalf("role budget = %#v, %v; want %#v", actual, err, expected)
		}
		config, err := budget.AdmissionConfig(expected.Role)
		wantConfig := outbound.ProcessAdmissionConfig{
			Role:                expected.Role,
			DataPoolMax:         expected.DataPoolMax,
			CoordinationPoolMax: 2,
			DBConnectionBudget:  20,
			ReservedConnections: 7,
			BudgetFingerprint:   fingerprint,
		}
		if err != nil || config != wantConfig {
			test.Fatalf("admission config = %#v, %v; want %#v", config, err, wantConfig)
		}
		active += actual.Active
		bulk += actual.Bulk
		actual.CPU = 99
		again, err := budget.ForRole(expected.Role)
		if err != nil || again != expected {
			test.Fatalf("returned role mutation changed manifest: %#v, %v", again, err)
		}
	}
	if active != 4 || budget.BaselineActive != 4 || bulk != 3 || budget.BaselineBulk != 2 {
		test.Fatal("role authorizations must preserve four active and two shared bulk slots")
	}
}

func TestBudgetFingerprintBindsCanonicalWholeManifest(test *testing.T) {
	budget := localBudgetFixture()
	before := budgetJSON(test, budget)
	digest := sha256.Sum256(before)
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	actual, err := budget.Fingerprint()
	if err != nil || actual != want || len(actual) != 43 {
		test.Fatal("fingerprint does not bind the whole canonical manifest", actual, err)
	}
	budget.Roles[0], budget.Roles[2] = budget.Roles[2], budget.Roles[0]
	reordered := budgetJSON(test, budget)
	if got, err := budget.Fingerprint(); err != nil || got != actual || !bytes.Equal(reordered, budgetJSON(test, budget)) {
		test.Fatal("fingerprint depends on role ordering or mutates input", got, err)
	}
	for _, change := range []func(*executionprofile.Budget){
		func(budget *executionprofile.Budget) { budget.BaselineConnections++ },
		func(budget *executionprofile.Budget) { budget.BaselineCPU++ },
		func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes++ },
		func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes++ },
		func(budget *executionprofile.Budget) { budget.Roles[0].Queue-- },
	} {
		changed := localBudgetFixture()
		change(&changed)
		if got, err := changed.Fingerprint(); err != nil || got == actual {
			test.Fatal("different valid budget shares a fingerprint", got, err)
		}
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		if len(outbound.BoundedDocumentsControlPrefix+string(role)+"/"+actual) > 63 {
			test.Fatal("control witness exceeds the PostgreSQL application-name bound")
		}
	}
	budget.Version = 0
	if got, err := budget.Fingerprint(); got != "" || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("invalid manifest received a fingerprint", got, err)
	}
}

func TestBudgetValidationRejectsInvalidDeclarations(test *testing.T) {
	testCases := []struct {
		name   string
		change func(*executionprofile.Budget)
	}{
		{"missing version", func(budget *executionprofile.Budget) { budget.Version = 0 }},
		{"missing profile", func(budget *executionprofile.Budget) { budget.Profile = "" }},
		{"active six", func(budget *executionprofile.Budget) { budget.BaselineActive = 6 }},
		{"bulk three", func(budget *executionprofile.Budget) { budget.BaselineBulk = 3 }},
		{"missing connection baseline", func(budget *executionprofile.Budget) { budget.BaselineConnections = 0 }},
		{"negative connection baseline", func(budget *executionprofile.Budget) { budget.BaselineConnections = -1 }},
		{"missing operator reserve", func(budget *executionprofile.Budget) { budget.ReservedConnections = 0 }},
		{"negative operator reserve", func(budget *executionprofile.Budget) { budget.ReservedConnections = -1 }},
		{"missing CPU baseline", func(budget *executionprofile.Budget) { budget.BaselineCPU = 0 }},
		{"missing memory baseline", func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes = 0 }},
		{"missing spool baseline", func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes = 0 }},
		{"negative CPU baseline", func(budget *executionprofile.Budget) { budget.BaselineCPU = -1 }},
		{"negative memory baseline", func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes = -1 }},
		{"negative spool baseline", func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes = -1 }},
		{"missing roles", func(budget *executionprofile.Budget) { budget.Roles = nil }},
		{"two roles", func(budget *executionprofile.Budget) { budget.Roles = budget.Roles[:2] }},
		{"extra role", func(budget *executionprofile.Budget) { budget.Roles = append(budget.Roles, budget.Roles[0]) }},
		{"duplicate role", func(budget *executionprofile.Budget) { budget.Roles[2].Role = outbound.ProcessAPI }},
		{"unrecognized role", func(budget *executionprofile.Budget) { budget.Roles[2].Role = "other" }},
		{"missing instances", func(budget *executionprofile.Budget) { budget.Roles[0].Instances = 0 }},
		{"second instance", func(budget *executionprofile.Budget) { budget.Roles[0].Instances = 2 }},
		{"API active one", func(budget *executionprofile.Budget) { budget.Roles[0].Active = 1 }},
		{"MCP active two", func(budget *executionprofile.Budget) { budget.Roles[1].Active = 2 }},
		{"worker active two", func(budget *executionprofile.Budget) { budget.Roles[2].Active = 2 }},
		{"missing bulk", func(budget *executionprofile.Budget) { budget.Roles[0].Bulk = 0 }},
		{"bulk two", func(budget *executionprofile.Budget) { budget.Roles[0].Bulk = 2 }},
		{"coordination one", func(budget *executionprofile.Budget) { budget.Roles[0].CoordinationPoolMax = 1 }},
		{"coordination three", func(budget *executionprofile.Budget) { budget.Roles[0].CoordinationPoolMax = 3 }},
		{"missing data pool", func(budget *executionprofile.Budget) { budget.Roles[0].DataPoolMax = 0 }},
		{"missing renewal reserve", func(budget *executionprofile.Budget) { budget.Roles[0].RenewalReserve = 0 }},
		{"negative renewal reserve", func(budget *executionprofile.Budget) { budget.Roles[0].RenewalReserve = -1 }},
		{"data pool omits reserve", func(budget *executionprofile.Budget) { budget.Roles[0].DataPoolMax = 2 }},
		{"larger renewal reserve", func(budget *executionprofile.Budget) { budget.Roles[0].RenewalReserve = 2 }},
		{"missing queue", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = 0 }},
		{"negative queue", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = -1 }},
		{"queue seventeen", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = 9 }},
		{"missing CPU", func(budget *executionprofile.Budget) { budget.Roles[0].CPU = 0 }},
		{"missing memory", func(budget *executionprofile.Budget) { budget.Roles[0].MemoryBytes = 0 }},
		{"missing spool", func(budget *executionprofile.Budget) { budget.Roles[0].SpoolBytes = 0 }},
		{"negative CPU", func(budget *executionprofile.Budget) { budget.Roles[0].CPU = -1 }},
		{"negative memory", func(budget *executionprofile.Budget) { budget.Roles[0].MemoryBytes = -1 }},
		{"negative spool", func(budget *executionprofile.Budget) { budget.Roles[0].SpoolBytes = -1 }},
		{"CPU aggregate", func(budget *executionprofile.Budget) { budget.BaselineCPU = 3 }},
		{"memory aggregate", func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes = 575 << 20 }},
		{"spool aggregate", func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes = 23 << 20 }},
		{"connection aggregate", func(budget *executionprofile.Budget) { budget.BaselineConnections = 19 }},
		{"operator reserve counted", func(budget *executionprofile.Budget) { budget.ReservedConnections = 8 }},
		{"all data pools counted", func(budget *executionprofile.Budget) { budget.Roles[2].DataPoolMax = 3 }},
		{"CPU overflow", func(budget *executionprofile.Budget) {
			budget.BaselineCPU = math.MaxInt64
			budget.Roles[0].CPU = math.MaxInt64
		}},
		{"memory overflow", func(budget *executionprofile.Budget) {
			budget.BaselineMemoryBytes = math.MaxInt64
			budget.Roles[0].MemoryBytes = math.MaxInt64
		}},
		{"spool overflow", func(budget *executionprofile.Budget) {
			budget.BaselineSpoolBytes = math.MaxInt64
			budget.Roles[0].SpoolBytes = math.MaxInt64
		}},
		{"connection overflow", func(budget *executionprofile.Budget) {
			budget.BaselineConnections = math.MaxInt32
			budget.ReservedConnections = math.MaxInt32
		}},
		{"data pool overflow", func(budget *executionprofile.Budget) {
			budget.BaselineConnections = math.MaxInt32
			budget.Roles[0].DataPoolMax = math.MaxInt32
		}},
		{"renewal reserve overflow", func(budget *executionprofile.Budget) {
			budget.Roles[0].DataPoolMax = math.MaxInt32
			budget.Roles[0].RenewalReserve = math.MaxInt32
		}},
		{"queue overflow", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = math.MaxInt32 }},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			budget := localBudgetFixture()
			testCase.change(&budget)
			if err := budget.Validate(); !errors.Is(err, domain.ErrValidation) {
				test.Fatalf("Validate() = %v; want validation", err)
			}
			requireBudgetRejection(test, budgetJSON(test, budget), domain.ErrValidation)
			if role, err := budget.ForRole(outbound.ProcessAPI); !errors.Is(err, domain.ErrValidation) || role != (executionprofile.RoleBudget{}) {
				test.Fatalf("invalid budget returned a role: %#v, %v", role, err)
			}
			if config, err := budget.AdmissionConfig(outbound.ProcessAPI); !errors.Is(err, domain.ErrValidation) || config != (outbound.ProcessAdmissionConfig{}) {
				test.Fatalf("invalid budget returned admission config: %#v, %v", config, err)
			}
		})
	}
}

func TestBudgetUnsupportedAndUnknownRole(test *testing.T) {
	for _, change := range []func(*executionprofile.Budget){
		func(budget *executionprofile.Budget) { budget.Version = 2 },
		func(budget *executionprofile.Budget) { budget.Profile = "embedded" },
	} {
		budget := localBudgetFixture()
		change(&budget)
		requireBudgetRejection(test, budgetJSON(test, budget), errors.ErrUnsupported)
		if err := budget.Validate(); !errors.Is(err, errors.ErrUnsupported) {
			test.Fatalf("Validate() = %v; want unsupported", err)
		}
	}
	for _, role := range []outbound.ProcessRole{"", "API", "other"} {
		budget := localBudgetFixture()
		if result, err := budget.ForRole(role); !errors.Is(err, domain.ErrValidation) || result != (executionprofile.RoleBudget{}) {
			test.Fatalf("ForRole() = %#v, %v; want zero role and validation", result, err)
		}
		if config, err := budget.AdmissionConfig(role); !errors.Is(err, domain.ErrValidation) || config != (outbound.ProcessAdmissionConfig{}) {
			test.Fatalf("AdmissionConfig() = %#v, %v; want zero config and validation", config, err)
		}
	}
	if err := (executionprofile.Budget{}).Validate(); !errors.Is(err, domain.ErrValidation) {
		test.Fatalf("zero budget = %v; want validation, not an embedded default", err)
	}
}

func TestBudgetParseRejectsNonStrictJSON(test *testing.T) {
	valid := string(budgetJSON(test, localBudgetFixture()))
	roles := string(jsonValue(test, localBudgetFixture().Roles))
	api := string(jsonValue(test, localBudgetFixture().Roles[0]))
	testCases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"null", "null"},
		{"array", "[]"},
		{"number", "1"},
		{"string", `"manifest"`},
		{"partial", `{"version":1}`},
		{"truncated", valid[:len(valid)-1]},
		{"comment", valid + "// comment"},
		{"trailing object", valid + "{}"},
		{"trailing array", valid + "[]"},
		{"trailing null", valid + "null"},
		{"trailing number", valid + "1"},
		{"trailing garbage", valid + "!"},
		{"unknown root field", strings.Replace(valid, `"version":1`, `"credentials":"never-print-this","version":1`, 1)},
		{"unknown role field", strings.Replace(valid, `"instances":1`, `"unknown":1,"instances":1`, 1)},
		{"wrong case root", strings.Replace(valid, `"version":1`, `"Version":1`, 1)},
		{"wrong case role", strings.Replace(valid, `"instances":1`, `"Instances":1`, 1)},
		{"duplicate root", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1)},
		{"escaped duplicate root", strings.Replace(valid, `"version":1`, `"version":1,"\u0076ersion":1`, 1)},
		{"duplicate role", strings.Replace(valid, `"instances":1`, `"instances":1,"instances":1`, 1)},
		{"escaped duplicate role", strings.Replace(valid, `"instances":1`, `"instances":1,"\u0069nstances":1`, 1)},
		{"version string", strings.Replace(valid, `"version":1`, `"version":"1"`, 1)},
		{"version fraction", strings.Replace(valid, `"version":1`, `"version":1.0`, 1)},
		{"CPU fraction", strings.Replace(valid, `"cpu":1`, `"cpu":1.5`, 1)},
		{"CPU exponent", strings.Replace(valid, `"cpu":1`, `"cpu":1e0`, 1)},
		{"int64 overflow", strings.Replace(valid, `"cpu":1`, `"cpu":9223372036854775808`, 1)},
		{"int64 underflow", strings.Replace(valid, `"cpu":1`, `"cpu":-9223372036854775809`, 1)},
		{"int32 overflow", strings.Replace(valid, `"data_pool_max":3`, `"data_pool_max":2147483648`, 1)},
		{"int32 underflow", strings.Replace(valid, `"data_pool_max":3`, `"data_pool_max":-2147483649`, 1)},
		{"version overflow", strings.Replace(valid, `"version":1`, `"version":18446744073709551616`, 1)},
		{"roles object", strings.Replace(valid, roles, `{}`, 1)},
		{"roles string", strings.Replace(valid, roles, `"api"`, 1)},
		{"role scalar", strings.Replace(valid, api, `1`, 1)},
		{"role null", strings.Replace(valid, api, `null`, 1)},
		{"invalid UTF-8", strings.Replace(valid, `"role":"api"`, "\"role\":\"ap\xffi\"", 1)},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			requireBudgetRejection(test, []byte(testCase.raw), domain.ErrValidation)
		})
	}
}

func TestBudgetParseRequiresEveryFieldAndRejectsNull(test *testing.T) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(budgetJSON(test, localBudgetFixture()), &root); err != nil {
		test.Fatal(err)
	}
	rootFields := make([]string, 0, len(root))
	for field := range root {
		rootFields = append(rootFields, field)
	}
	for _, field := range rootFields {
		original := root[field]
		test.Run("missing root "+field, func(test *testing.T) {
			delete(root, field)
			requireBudgetRejection(test, jsonValue(test, root), domain.ErrValidation)
			root[field] = original
		})
		test.Run("null root "+field, func(test *testing.T) {
			root[field] = json.RawMessage("null")
			requireBudgetRejection(test, jsonValue(test, root), domain.ErrValidation)
			root[field] = original
		})
	}
	var roles []map[string]json.RawMessage
	if err := json.Unmarshal(root["roles"], &roles); err != nil {
		test.Fatal(err)
	}
	roleFields := make([]string, 0, len(roles[0]))
	for field := range roles[0] {
		roleFields = append(roleFields, field)
	}
	for _, field := range roleFields {
		original := roles[0][field]
		test.Run("missing role "+field, func(test *testing.T) {
			delete(roles[0], field)
			root["roles"] = jsonValue(test, roles)
			requireBudgetRejection(test, jsonValue(test, root), domain.ErrValidation)
			roles[0][field] = original
		})
		test.Run("null role "+field, func(test *testing.T) {
			roles[0][field] = json.RawMessage("null")
			root["roles"] = jsonValue(test, roles)
			requireBudgetRejection(test, jsonValue(test, root), domain.ErrValidation)
			roles[0][field] = original
		})
	}
}

func TestBudgetExactDeclaredBoundaries(test *testing.T) {
	budget := localBudgetFixture()
	budget.Roles[0].Queue = 8
	budget.BaselineMemoryBytes = 576 << 20
	budget.BaselineSpoolBytes = 24 << 20
	if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
		test.Fatalf("exact aggregate boundaries rejected: %v", err)
	}
	budget.Roles[0].RenewalReserve = 2
	budget.Roles[0].DataPoolMax = 4
	budget.BaselineConnections = 21
	if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
		test.Fatalf("explicit larger renewal reserve was double-counted: %v", err)
	}
	budget = localBudgetFixture()
	budget.BaselineCPU = math.MaxInt64
	budget.BaselineMemoryBytes = math.MaxInt64
	budget.BaselineSpoolBytes = math.MaxInt64
	budget.Roles[0].CPU = math.MaxInt64 - 3
	budget.Roles[0].MemoryBytes = math.MaxInt64 - (384 << 20)
	budget.Roles[0].SpoolBytes = math.MaxInt64 - (16 << 20)
	if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
		test.Fatalf("representable exact int64 boundary rejected: %v", err)
	}
	budget = localBudgetFixture()
	budget.BaselineConnections = math.MaxInt32
	budget.ReservedConnections = math.MaxInt32 - 13
	if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
		test.Fatalf("representable exact int32 connection boundary rejected: %v", err)
	}
	budget = localBudgetFixture()
	budget.Roles[0], budget.Roles[2] = budget.Roles[2], budget.Roles[0]
	if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
		test.Fatalf("role order should not change the roster: %v", err)
	}
}

func TestBudgetManifestSizeBound(test *testing.T) {
	raw := budgetJSON(test, localBudgetFixture())
	raw = append(raw, bytes.Repeat([]byte(" "), executionprofile.MaxManifestBytes-len(raw))...)
	if _, err := executionprofile.Parse(raw); err != nil {
		test.Fatalf("exact 64 KiB manifest rejected: %v", err)
	}
	requireBudgetRejection(test, append(raw, ' '), domain.ErrValidation)
}

func TestBudgetErrorsDoNotEchoInput(test *testing.T) {
	const secret = "never-print-this-private-value"
	valid := string(budgetJSON(test, localBudgetFixture()))
	for _, raw := range []string{
		strings.Replace(valid, `"version":1`, `"`+secret+`":1,"version":1`, 1),
		strings.Replace(valid, `"cpu":1`, `"cpu":"`+secret+`"`, 1),
		strings.Replace(valid, `"profile":"bounded-documents-v1"`, `"profile":"`+secret+`"`, 1),
		strings.Replace(valid, `"role":"api"`, `"role":"`+secret+`"`, 1),
	} {
		_, err := executionprofile.Parse([]byte(raw))
		if err == nil || strings.Contains(err.Error(), secret) {
			test.Fatalf("error should reject input without echoing its values")
		}
	}
}

func localBudgetFixture() executionprofile.Budget {
	return executionprofile.Budget{
		Version:             executionprofile.ManifestVersion,
		Profile:             outbound.BoundedDocumentsProfile,
		BaselineActive:      4,
		BaselineBulk:        2,
		BaselineConnections: 20,
		ReservedConnections: 7,
		BaselineCPU:         4,
		BaselineMemoryBytes: 800 << 20,
		BaselineSpoolBytes:  64 << 20,
		Roles: []executionprofile.RoleBudget{
			{Role: outbound.ProcessAPI, Instances: 1, Active: 2, Bulk: 1, DataPoolMax: 3, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
			{Role: outbound.ProcessMCP, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
			{Role: outbound.ProcessWorker, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 2, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
		},
	}
}

func budgetJSON(test testing.TB, budget executionprofile.Budget) []byte {
	test.Helper()
	return jsonValue(test, budget)
}

func jsonValue(test testing.TB, value any) []byte {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func requireBudgetRejection(test testing.TB, raw []byte, want error) {
	test.Helper()
	budget, err := executionprofile.Parse(raw)
	if !errors.Is(err, want) || !reflect.DeepEqual(budget, executionprofile.Budget{}) {
		test.Fatalf("Parse() = %#v, %v; want zero budget and %v", budget, err, want)
	}
}
