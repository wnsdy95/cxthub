package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type IdentityKeyRequest struct {
	EnterpriseID                    string
	After, Operation, Actor, Reason string
	Limit                           int
	Apply                           bool
}

// RewrapIdentityKeys is an operator workflow. It never changes membership,
// connection revisions, proof validity or the SAML signing certificate.
func RewrapIdentityKeys(ctx context.Context, st outbound.IdentityKeyStore, cipher outbound.IdentityKeyCipher, in IdentityKeyRequest) (domain.IdentityKeyBatch, error) {
	var out domain.IdentityKeyBatch
	if st == nil || cipher == nil || cipher.ActiveKeyID() == "" {
		return out, domain.ErrValidation
	}
	if in.EnterpriseID != "" && domain.ValidateEnterpriseID(in.EnterpriseID) != nil {
		return out, domain.ErrValidation
	}
	if _, _, err := domain.IdentitySecretCursor(in.After); err != nil {
		return out, err
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	if in.Limit < 1 || in.Limit > 100 {
		return out, domain.ErrValidation
	}
	if in.Apply && (!validKeyAuditInput(in.Operation, 128) || !validKeyAuditInput(in.Actor, 128) || !validKeyAuditInput(in.Reason, 512)) {
		return out, domain.ErrValidation
	}
	run := func(ctx context.Context) error {
		if in.Apply {
			prior, err := st.GetIdentityKeyBatch(ctx, in.Operation, in.After)
			if err == nil {
				if prior.EnterpriseID != in.EnterpriseID || prior.ActiveKey != cipher.ActiveKeyID() || prior.Limit != in.Limit || prior.Actor != in.Actor || prior.Reason != in.Reason {
					return domain.ErrConflict
				}
				out = prior
				return nil
			}
			if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		}
		rows, err := st.ListIdentitySecrets(ctx, in.EnterpriseID, in.After, in.Limit+1)
		if err != nil {
			return err
		}
		out = domain.IdentityKeyBatch{EnterpriseID: in.EnterpriseID, Operation: in.Operation, Actor: in.Actor, Reason: in.Reason, ActiveKey: cipher.ActiveKeyID(), After: in.After, Next: in.After, Limit: in.Limit, Keys: map[string]int{}, Complete: len(rows) <= in.Limit, Applied: in.Apply, CreatedAt: time.Now().UTC()}
		if len(rows) > in.Limit {
			rows = rows[:in.Limit]
		}
		for _, row := range rows {
			purpose, err := row.Purpose()
			if err != nil {
				return err
			}
			key, err := cipher.Inspect(purpose, row.Sealed)
			if err != nil {
				return domain.ErrIntegrity
			}
			out.Keys[key]++
			out.Scanned++
			out.Next = row.Cursor()
			if key == cipher.ActiveKeyID() {
				continue
			}
			out.Candidates++
			if !in.Apply {
				continue
			}
			plain, err := cipher.Open(purpose, row.Sealed)
			if err != nil {
				return domain.ErrIntegrity
			}
			sealed, err := cipher.Seal(purpose, plain)
			if err != nil {
				return domain.ErrIntegrity
			}
			if err = st.ReplaceIdentitySecret(ctx, row, sealed); err != nil {
				return err
			}
			out.Changed++
		}
		if in.Apply {
			return st.PutIdentityKeyBatch(ctx, out)
		}
		return nil
	}
	var err error
	if in.Apply {
		err = st.WithinIdentity(ctx, run)
	} else {
		err = run(ctx)
	}
	if err != nil {
		return domain.IdentityKeyBatch{}, err
	}
	return out, nil
}

func validKeyAuditInput(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && !strings.ContainsAny(s, "\x00\r\n")
}
