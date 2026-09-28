//go:build postgres

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/federation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func runIdentityKeys(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("identity-keys", flag.ContinueOnError)
	f.SetOutput(stderr)
	var in app.IdentityKeyRequest
	f.StringVar(&in.EnterpriseID, "enterprise", "", "optional immutable Enterprise ID; default examines all")
	f.StringVar(&in.After, "after", "", "resume cursor from the preceding committed batch")
	f.IntVar(&in.Limit, "limit", 100, "maximum values examined (1..100)")
	f.StringVar(&in.Operation, "operation", "", "stable rotation operation ID; reuse for a retry")
	f.StringVar(&in.Actor, "actor", "", "operator identity for audit")
	f.StringVar(&in.Reason, "reason", "", "rotation reason; never include credentials")
	f.BoolVar(&in.Apply, "apply", false, "rewrap this batch; default only verifies and reports")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected identity-keys arguments")
	}
	v, err := federation.ConfiguredVault(os.Getenv("CXT_IDENTITY_ENCRYPTION_KEY"), os.Getenv("CXT_IDENTITY_ENCRYPTION_KEYRING"))
	if err != nil || v == nil {
		return fmt.Errorf("valid identity encryption key configuration is required")
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
	b, err := app.RewrapIdentityKeys(ctx, st, v, in)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrIntegrity):
			return fmt.Errorf("identity key batch failed: unreadable or invalid credential; restore required read keys and retry; no changes committed")
		case errors.Is(err, domain.ErrValidation):
			return fmt.Errorf("invalid batch request; apply requires operation, actor, reason and limit 1..100")
		case errors.Is(err, domain.ErrConflict):
			return fmt.Errorf("identity key batch conflicts with an earlier receipt or concurrent edit; no changes committed")
		default:
			return fmt.Errorf("identity key batch outcome is unconfirmed; retry with the same operation and cursor to recover its durable receipt")
		}
	}
	return json.NewEncoder(stdout).Encode(b)
}
