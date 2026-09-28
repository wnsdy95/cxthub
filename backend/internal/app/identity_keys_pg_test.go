//go:build postgres

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/federation"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func rotationVault(t *testing.T, legacy bool) *federation.Vault {
	t.Helper()
	old := ""
	if legacy {
		old = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	}
	config, _ := json.Marshal(map[string]any{"active": "next", "keys": map[string]string{"next": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))}})
	v, err := federation.ConfiguredVault(old, string(config))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func keyFixture(t *testing.T) (federationFixture, string, domain.SAMLConnection) {
	t.Helper()
	f := newFederationFixture(t)
	state := f.start(t, f.session.Token, f.team.owner.ID)
	c := domain.SAMLConnection{EnterpriseID: f.enterprise.ID, Revision: domain.NewID(""), Issuer: "https://idp.example.test", Domain: f.input.Domain, Metadata: "synthetic-metadata", Certificate: "synthetic-certificate"}
	c.PrivateKey, _ = f.vault.Seal("saml:"+c.EnterpriseID+":"+c.Revision, "synthetic-signing-key")
	if err := f.st.WithinIdentity(context.Background(), func(ctx context.Context) error { return f.st.PutSAMLConnection(ctx, c) }); err != nil {
		t.Fatal(err)
	}
	return f, state, c
}
func keyRequest(f federationFixture) IdentityKeyRequest {
	return IdentityKeyRequest{EnterpriseID: f.enterprise.ID, Operation: domain.NewID("rotation_"), Actor: "fixture-operator", Reason: "planned key rotation", Limit: 1, Apply: true}
}

func TestPGIdentityKeyRotationPreservesOutstandingLoginAndCertificate(t *testing.T) {
	f, state, cert := keyFixture(t)
	other, _, _ := keyFixture(t)
	otherBefore, _ := other.st.ListIdentitySecrets(context.Background(), other.enterprise.ID, "", 100)
	ctx := context.Background()
	v := rotationVault(t, true)
	req := keyRequest(f)
	before, err := f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	if err != nil || len(before) != 3 {
		t.Fatal(len(before), err)
	}
	dry := req
	dry.Apply = false
	dry.Limit = 100
	b, err := RewrapIdentityKeys(ctx, f.st, v, dry)
	if err != nil || b.Candidates != 3 || b.Changed != 0 || !b.Complete {
		t.Fatal(b, err)
	}
	unchanged, _ := f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	if !reflect.DeepEqual(before, unchanged) {
		t.Fatal("inspection mutated credentials")
	}
	if _, err := f.st.GetIdentityKeyBatch(ctx, req.Operation, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("inspection wrote audit")
	}
	count := 0
	for {
		b, err = RewrapIdentityKeys(ctx, f.st, v, req)
		if err != nil {
			t.Fatal(err)
		}
		if b.Changed != 1 || b.Scanned != 1 {
			t.Fatal(b)
		}
		count += b.Changed
		// A retry returns the exact committed receipt; it does not rotate again.
		retry, err := RewrapIdentityKeys(ctx, f.st, v, req)
		if err != nil || !reflect.DeepEqual(retry, b) {
			t.Fatal("unstable receipt", err)
		}
		conflict := req
		conflict.Reason = "different operation intent"
		if _, err := RewrapIdentityKeys(ctx, f.st, v, conflict); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("receipt intent changed", err)
		}
		if b.Complete {
			break
		}
		req.After = b.Next
	}
	if count != 3 {
		t.Fatal(count)
	}
	// Restarted process can finish the pre-rotation OIDC attempt with only new key.
	onlyNew := rotationVault(t, false)
	f.team.identity.WithOIDC(f.provider, onlyNew, "https://cxthub.example.test")
	if _, err := f.team.identity.CompleteOIDC(systemTestContext(), f.session.Token, state, "code"); err != nil {
		t.Fatal("pending login lost", err)
	}
	after, err := f.st.GetSAMLConnection(ctx, f.enterprise.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != cert.Revision || after.Certificate != cert.Certificate || after.Metadata != cert.Metadata {
		t.Fatal("SP identity changed")
	}
	plain, err := onlyNew.Open("saml:"+after.EnterpriseID+":"+after.Revision, after.PrivateKey)
	if err != nil || plain != "synthetic-signing-key" {
		t.Fatal("SAML key lost", err)
	}
	dry.After = ""
	b, err = RewrapIdentityKeys(ctx, f.st, onlyNew, dry)
	if err != nil || b.Candidates != 0 || b.Keys["next"] != 3 {
		t.Fatal(b, err)
	}
	otherAfter, _ := other.st.ListIdentitySecrets(ctx, other.enterprise.ID, "", 100)
	if !reflect.DeepEqual(otherBefore, otherAfter) {
		t.Fatal("scoped rotation changed another enterprise")
	}
}

type failingKeyAudit struct{ outbound.IdentityKeyStore }

func (f failingKeyAudit) PutIdentityKeyBatch(context.Context, domain.IdentityKeyBatch) error {
	return errors.New("synthetic audit failure")
}

func TestPGIdentityKeyRotationRollsBackOnMissingKeyCorruptionAndAuditFailure(t *testing.T) {
	f, _, _ := keyFixture(t)
	ctx := context.Background()
	req := keyRequest(f)
	req.Limit = 100
	before, _ := f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	if _, err := RewrapIdentityKeys(ctx, f.st, rotationVault(t, false), req); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal(err)
	}
	if _, err := RewrapIdentityKeys(ctx, failingKeyAudit{f.st}, rotationVault(t, true), req); err == nil {
		t.Fatal("audit failure ignored")
	}
	still, _ := f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	if !reflect.DeepEqual(before, still) {
		t.Fatal("failed batch partially published")
	}
	// Corrupt the last value: earlier replacements must roll back too.
	last := before[len(before)-1]
	if err := f.st.WithinIdentity(ctx, func(ctx context.Context) error { return f.st.ReplaceIdentitySecret(ctx, last, "v2.next.invalid") }); err != nil {
		t.Fatal(err)
	}
	if _, err := RewrapIdentityKeys(ctx, f.st, rotationVault(t, true), req); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("active-key header bypassed authentication", err)
	}
	still, _ = f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	if !reflect.DeepEqual(before[:2], still[:2]) {
		t.Fatal("earlier rows escaped rollback")
	}
	if _, err := f.st.GetIdentityKeyBatch(ctx, req.Operation, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("failed batch has receipt")
	}
	if err := f.st.ReplaceIdentitySecret(ctx, before[0], "anything"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("mutation outside identity transaction", err)
	}
}

func TestPGIdentityKeyRotationConcurrentRetryAndEditor(t *testing.T) {
	f, _, _ := keyFixture(t)
	ctx := context.Background()
	req := keyRequest(f)
	req.Limit = 100
	v := rotationVault(t, true)
	var wg sync.WaitGroup
	results := make(chan domain.IdentityKeyBatch, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); b, e := RewrapIdentityKeys(ctx, f.st, v, req); results <- b; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	first := <-results
	second := <-results
	if !reflect.DeepEqual(first, second) || first.Changed != 3 {
		t.Fatal("concurrent replay changed receipt")
	}
	rows, _ := f.st.ListIdentitySecrets(ctx, f.enterprise.ID, "", 100)
	old := rows[0]
	if err := f.st.WithinIdentity(ctx, func(ctx context.Context) error {
		if err := f.st.ReplaceIdentitySecret(ctx, old, "simulated-new-editor-value"); err != nil {
			return err
		}
		if err := f.st.ReplaceIdentitySecret(ctx, old, "stale-rewrap"); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("stale ciphertext overwritten", err)
		}
		return domain.ErrConflict // restore the fixture on rollback
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	// Ordinary identity writes serialize with a rewrap batch.
	locked, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- f.st.WithinIdentity(ctx, func(context.Context) error { close(locked); <-release; return nil })
	}()
	<-locked
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	req.Operation = domain.NewID("rotation_")
	if _, err := RewrapIdentityKeys(short, f.st, v, req); err == nil {
		t.Fatal("bypassed identity writer lock")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
