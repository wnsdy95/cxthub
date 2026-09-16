//go:build postgres

// cxt-admin is an operator-only provisioning tool. It is not part of the client
// CLI or the MCP surface; access requires the deployment's database credential.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"io"
	"os"
	"time"
)

func run() error {
	ns := flag.String("namespace", "", "immutable namespace ID")
	policyFile := flag.String("policy", "", "JSON StoragePolicy file")
	operation := flag.String("operation", "", "stable idempotency ID; reuse when retrying")
	expected := flag.Int64("expect", -1, "expected policy revision")
	actor := flag.String("actor", "", "operator identity for audit")
	reason := flag.String("reason", "", "provisioning reason for audit")
	apply := flag.Bool("apply", false, "apply after reviewing the dry-run output")
	flag.Parse()
	if *policyFile == "" || domain.ValidateNamespaceID(*ns) != nil {
		return fmt.Errorf("namespace and policy file are required")
	}
	f, err := os.Open(*policyFile)
	if err != nil {
		return err
	}
	defer f.Close()
	var p domain.StoragePolicy
	dec := json.NewDecoder(io.LimitReader(f, 16<<10))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("policy must be one JSON object")
	}
	if err = p.Validate(); err != nil {
		return err
	}
	dsn := os.Getenv("CXT_POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("CXT_POSTGRES_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database connection failed")
	}
	defer st.Close()
	now := time.Now().UTC()
	before, err := st.ReadStorageUsage(ctx, *ns, now.Add(-time.Hour), now)
	if err != nil {
		return err
	}
	if *apply {
		if *operation == "" || *actor == "" || *reason == "" || *expected < 0 {
			return fmt.Errorf("apply requires operation, actor, reason, and expected revision")
		}
		if err = st.ConfigureStoragePolicy(ctx, *ns, *operation, *expected, p, *actor, *reason); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Applied         bool                 `json:"applied"`
		Namespace       string               `json:"namespace"`
		CurrentRevision int64                `json:"previous_revision"`
		CurrentBytes    int64                `json:"current_bytes"`
		Policy          domain.StoragePolicy `json:"requested_policy"`
	}{*apply, *ns, before.PolicyRevision, before.CurrentBytes, p})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
