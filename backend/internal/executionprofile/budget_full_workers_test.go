package executionprofile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestBoundedWorkersBudgetPreservesDocumentProfileLimits(test *testing.T) {
	if outbound.BoundedWorkersProfile != "bounded-workers-v1" || outbound.BoundedDocumentsProfile != "bounded-documents-v1" {
		test.Fatal("execution profile names changed")
	}
	documents := localBudgetFixture()
	workers := localBudgetFixture()
	workers.Profile = outbound.BoundedWorkersProfile
	parsed, err := executionprofile.Parse(budgetJSON(test, workers))
	if err != nil || !reflect.DeepEqual(parsed, workers) {
		test.Fatalf("full worker budget = %#v, %v; want %#v", parsed, err, workers)
	}
	normalized := parsed
	normalized.Profile = outbound.BoundedDocumentsProfile
	if !reflect.DeepEqual(normalized, documents) {
		test.Fatal("full worker profile changed the existing resource envelopes")
	}
	for _, expected := range documents.Roles {
		actual, err := parsed.ForRole(expected.Role)
		if err != nil || actual != expected {
			test.Fatalf("full worker role = %#v, %v; want %#v", actual, err, expected)
		}
		documentConfig, err := documents.AdmissionConfig(expected.Role)
		if err != nil {
			test.Fatal(err)
		}
		workerConfig, err := parsed.AdmissionConfig(expected.Role)
		if err != nil || workerConfig.BudgetFingerprint == documentConfig.BudgetFingerprint {
			test.Fatal("full worker admission did not bind the distinct manifest", workerConfig, err)
		}
		workerConfig.BudgetFingerprint = documentConfig.BudgetFingerprint
		if workerConfig != documentConfig {
			test.Fatal("full worker admission changed the shared role or connection limits")
		}
	}
}

func TestBoundedWorkersBudgetFingerprintBindsProfileAndCanonicalRoles(test *testing.T) {
	budget := localBudgetFixture()
	documentsFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal(err)
	}
	budget.Profile = outbound.BoundedWorkersProfile
	digest := sha256.Sum256(budgetJSON(test, budget))
	expected := base64.RawURLEncoding.EncodeToString(digest[:])
	fingerprint, err := budget.Fingerprint()
	if err != nil || fingerprint != expected || fingerprint == documentsFingerprint || len(fingerprint) != 43 {
		test.Fatal("full worker fingerprint does not bind the canonical whole manifest", fingerprint, err)
	}
	budget.Roles[0], budget.Roles[2] = budget.Roles[2], budget.Roles[0]
	before := budgetJSON(test, budget)
	if actual, err := budget.Fingerprint(); err != nil || actual != fingerprint || !bytes.Equal(before, budgetJSON(test, budget)) {
		test.Fatal("full worker fingerprint depends on role ordering or mutates the manifest", actual, err)
	}
	if outbound.BoundedDocumentsControlPrefix != "cxthub/bd1/" {
		test.Fatal("full worker profile changed the shared PostgreSQL control namespace")
	}
	for _, role := range budget.Roles {
		config, err := budget.AdmissionConfig(role.Role)
		if err != nil || config.BudgetFingerprint != fingerprint {
			test.Fatal("role admission lost the full worker fingerprint", config, err)
		}
		if len(outbound.BoundedDocumentsControlPrefix+string(role.Role)+"/"+fingerprint) > 63 {
			test.Fatal("full worker control witness exceeds the PostgreSQL application-name bound")
		}
	}
}

func TestBoundedWorkersBudgetKeepsExistingValidationLimits(test *testing.T) {
	testCases := []struct {
		name   string
		change func(*executionprofile.Budget)
	}{
		{"active baseline", func(budget *executionprofile.Budget) { budget.BaselineActive = 6 }},
		{"bulk baseline", func(budget *executionprofile.Budget) { budget.BaselineBulk = 3 }},
		{"missing role", func(budget *executionprofile.Budget) { budget.Roles = budget.Roles[:2] }},
		{"duplicate role", func(budget *executionprofile.Budget) { budget.Roles[2].Role = outbound.ProcessAPI }},
		{"extra instance", func(budget *executionprofile.Budget) { budget.Roles[2].Instances = 2 }},
		{"API active", func(budget *executionprofile.Budget) { budget.Roles[0].Active = 1 }},
		{"MCP active", func(budget *executionprofile.Budget) { budget.Roles[1].Active = 2 }},
		{"worker active", func(budget *executionprofile.Budget) { budget.Roles[2].Active = 2 }},
		{"worker bulk", func(budget *executionprofile.Budget) { budget.Roles[2].Bulk = 2 }},
		{"coordination sessions", func(budget *executionprofile.Budget) { budget.Roles[2].CoordinationPoolMax = 3 }},
		{"renewal reserve", func(budget *executionprofile.Budget) { budget.Roles[2].RenewalReserve = 0 }},
		{"data renewal headroom", func(budget *executionprofile.Budget) { budget.Roles[2].DataPoolMax = 1 }},
		{"missing CPU", func(budget *executionprofile.Budget) { budget.Roles[2].CPU = 0 }},
		{"missing memory", func(budget *executionprofile.Budget) { budget.Roles[2].MemoryBytes = 0 }},
		{"missing spool", func(budget *executionprofile.Budget) { budget.Roles[2].SpoolBytes = 0 }},
		{"missing queue", func(budget *executionprofile.Budget) { budget.Roles[2].Queue = 0 }},
		{"connection aggregate", func(budget *executionprofile.Budget) { budget.BaselineConnections = 19 }},
		{"operator reserve counted", func(budget *executionprofile.Budget) { budget.ReservedConnections = 8 }},
		{"worker pool counted", func(budget *executionprofile.Budget) { budget.Roles[2].DataPoolMax = 3 }},
		{"CPU aggregate", func(budget *executionprofile.Budget) { budget.BaselineCPU = 3 }},
		{"memory aggregate", func(budget *executionprofile.Budget) { budget.BaselineMemoryBytes = 575 << 20 }},
		{"spool aggregate", func(budget *executionprofile.Budget) { budget.BaselineSpoolBytes = 23 << 20 }},
		{"queue aggregate", func(budget *executionprofile.Budget) { budget.Roles[0].Queue = 9 }},
		{"CPU overflow", func(budget *executionprofile.Budget) {
			budget.BaselineCPU, budget.Roles[0].CPU = math.MaxInt64, math.MaxInt64
		}},
		{"memory overflow", func(budget *executionprofile.Budget) {
			budget.BaselineMemoryBytes, budget.Roles[0].MemoryBytes = math.MaxInt64, math.MaxInt64
		}},
		{"spool overflow", func(budget *executionprofile.Budget) {
			budget.BaselineSpoolBytes, budget.Roles[0].SpoolBytes = math.MaxInt64, math.MaxInt64
		}},
		{"connection overflow", func(budget *executionprofile.Budget) {
			budget.BaselineConnections, budget.ReservedConnections = math.MaxInt32, math.MaxInt32
		}},
		{"queue overflow", func(budget *executionprofile.Budget) { budget.Roles[2].Queue = math.MaxInt32 }},
	}
	for _, profileName := range []string{outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		for _, testCase := range testCases {
			test.Run(profileName+"/"+testCase.name, func(test *testing.T) {
				budget := localBudgetFixture()
				budget.Profile = profileName
				testCase.change(&budget)
				if err := budget.Validate(); !errors.Is(err, domain.ErrValidation) {
					test.Fatalf("Validate() = %v; want unchanged validation", err)
				}
				requireBudgetRejection(test, budgetJSON(test, budget), domain.ErrValidation)
				if config, err := budget.AdmissionConfig(outbound.ProcessWorker); !errors.Is(err, domain.ErrValidation) || config != (outbound.ProcessAdmissionConfig{}) {
					test.Fatal("invalid worker manifest returned an admission config", config, err)
				}
				if fingerprint, err := budget.Fingerprint(); !errors.Is(err, domain.ErrValidation) || fingerprint != "" {
					test.Fatal("invalid worker manifest returned a fingerprint", fingerprint, err)
				}
			})
		}
	}
}

func TestBoundedWorkersBudgetAcceptsExactExistingAggregateBoundaries(test *testing.T) {
	for _, profileName := range []string{outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		test.Run(profileName, func(test *testing.T) {
			budget := localBudgetFixture()
			budget.Profile = profileName
			budget.Roles[0].Queue = 8
			budget.BaselineMemoryBytes = 576 << 20
			budget.BaselineSpoolBytes = 24 << 20
			if _, err := executionprofile.Parse(budgetJSON(test, budget)); err != nil {
				test.Fatal("unchanged exact aggregate boundaries were rejected", err)
			}
		})
	}
}

func TestBoundedWorkersBudgetRequiresStrictExplicitProfile(test *testing.T) {
	budget := localBudgetFixture()
	budget.Profile = outbound.BoundedWorkersProfile
	valid := string(budgetJSON(test, budget))
	for _, profileName := range []string{"", "embedded", "bounded-workers-v2", "Bounded-workers-v1", " bounded-workers-v1 "} {
		budget.Profile = profileName
		want := errors.ErrUnsupported
		if profileName == "" {
			want = domain.ErrValidation
		}
		requireBudgetRejection(test, budgetJSON(test, budget), want)
	}
	budget.Profile, budget.Version = outbound.BoundedWorkersProfile, executionprofile.ManifestVersion+1
	requireBudgetRejection(test, budgetJSON(test, budget), errors.ErrUnsupported)
	for _, raw := range []string{
		strings.Replace(valid, `"profile":"bounded-workers-v1",`, "", 1),
		strings.Replace(valid, `"profile":"bounded-workers-v1"`, `"profile":null`, 1),
		strings.Replace(valid, `"profile":`, `"profile":"bounded-documents-v1","profile":`, 1),
		strings.Replace(valid, `"version":1`, `"workers":true,"version":1`, 1),
		strings.Replace(valid, `"instances":1`, `"instances":1,"instances":1`, 1),
		valid + `{}`,
	} {
		requireBudgetRejection(test, []byte(raw), domain.ErrValidation)
	}
}
