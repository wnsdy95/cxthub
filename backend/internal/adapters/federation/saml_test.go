package federation

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type samlFixture struct {
	settings outbound.SAMLSettings
	key      *rsa.PrivateKey
	cert     *x509.Certificate
	started  time.Time
}

func newSAMLFixture(t *testing.T) samlFixture {
	t.Helper()
	certificate, private, err := NewSAML().Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := pem.Decode([]byte(private))
	key, _ := x509.ParsePKCS1PrivateKey(kb.Bytes)
	cb, _ := pem.Decode([]byte(certificate))
	cert, _ := x509.ParseCertificate(cb.Bytes)
	md := `<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="https://idp.example.test"><md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + base64.StdEncoding.EncodeToString(cert.Raw) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.test/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`
	return samlFixture{outbound.SAMLSettings{Metadata: md, EntityID: "https://cxthub.example.test/saml/metadata", ACS: "https://cxthub.example.test/saml/acs", Certificate: certificate, PrivateKey: private}, key, cert, time.Now().UTC().Add(-time.Second)}
}
func (f samlFixture) assertion() saml.Assertion {
	now := time.Now().UTC()
	return saml.Assertion{ID: "assertion-one", Version: "2.0", IssueInstant: now, Issuer: saml.Issuer{Value: "https://idp.example.test"}, Subject: &saml.Subject{NameID: &saml.NameID{Format: string(saml.PersistentNameIDFormat), Value: "immutable-subject"}, SubjectConfirmations: []saml.SubjectConfirmation{{Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer", SubjectConfirmationData: &saml.SubjectConfirmationData{InResponseTo: "request-one", Recipient: f.settings.ACS, NotOnOrAfter: now.Add(5 * time.Minute)}}}}, Conditions: &saml.Conditions{NotBefore: now.Add(-time.Minute), NotOnOrAfter: now.Add(5 * time.Minute), AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: f.settings.EntityID}}}}, AuthnStatements: []saml.AuthnStatement{{AuthnInstant: now, AuthnContext: saml.AuthnContext{AuthnContextClassRef: &saml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}}}}}
}
func (f samlFixture) response(t *testing.T, a saml.Assertion, algorithm string) []byte {
	return f.responseID(t, a, algorithm, "request-one")
}
func (f samlFixture) responseID(t *testing.T, a saml.Assertion, algorithm, id string) []byte {
	t.Helper()
	r := saml.Response{ID: "response-one", Version: "2.0", IssueInstant: time.Now().UTC(), InResponseTo: id, Destination: f.settings.ACS, Issuer: &saml.Issuer{Value: "https://idp.example.test"}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}}
	el := r.Element()
	ae := a.Element()
	if algorithm != "" {
		sc, err := dsig.NewSigningContext(f.key, [][]byte{f.cert.Raw})
		if err != nil {
			t.Fatal(err)
		}
		sc.IdAttribute = "ID"
		sc.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
		if err = sc.SetSignatureMethod(algorithm); err != nil {
			t.Fatal(err)
		}
		ae, err = sc.SignEnveloped(ae)
		if err != nil {
			t.Fatal(err)
		}
	}
	el.AddChild(ae)
	doc := etree.NewDocument()
	doc.SetRoot(el)
	raw, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSAMLSPInitiatedSignedProfile(t *testing.T) {
	f := newSAMLFixture(t)
	p := NewSAML()
	ctx := context.Background()
	issuer, err := p.Validate(ctx, f.settings)
	if err != nil || issuer != "https://idp.example.test" {
		t.Fatal(issuer, err)
	}
	location, err := p.Authorize(ctx, f.settings, "request-one", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(location)
	q := u.Query()
	if u.Host != "idp.example.test" || q.Get("SigAlg") != dsig.RSASHA256SignatureMethod || q.Get("Signature") == "" || q.Get("RelayState") != strings.Repeat("a", 64) {
		t.Fatal("unsigned or wrong redirect")
	}
	compressed, _ := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer reader.Close()
	b, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var request saml.AuthnRequest
	if xml.Unmarshal(b, &request) != nil || request.ID != "request-one" || request.ForceAuthn == nil || !*request.ForceAuthn || request.AssertionConsumerServiceURL != f.settings.ACS {
		t.Fatal("wrong authentication request")
	}
	md, err := p.Metadata(ctx, f.settings)
	if err != nil || strings.Contains(md, `use="encryption"`) || strings.Contains(md, "SingleLogoutService") || !strings.Contains(md, `WantAssertionsSigned="true"`) {
		t.Fatal("wrong metadata", err)
	}
	raw := f.response(t, f.assertion(), dsig.RSASHA256SignatureMethod)
	proof, err := p.Verify(ctx, f.settings, "request-one", raw, f.started)
	if err != nil {
		t.Fatal("valid signed response rejected", err)
	}
	if proof.Subject != "immutable-subject" || proof.AssertionID != "assertion-one" || proof.Issuer != issuer || !proof.ExpiresAt.After(time.Now()) || len(proof.AMR) != 0 {
		t.Fatal("invalid proof", proof)
	}
	// Protocol verification alone deliberately does not claim replay consumption.
	// Application persistence must reject the second use of this assertion ID.
	if _, err = p.Verify(ctx, f.settings, "other-request", raw, f.started); err == nil {
		t.Fatal("unrelated request accepted")
	}
}

func TestSAMLRejectsSignedInvalidClaims(t *testing.T) {
	f := newSAMLFixture(t)
	cases := map[string]func(*saml.Assertion){
		"audience absent": func(a *saml.Assertion) { a.Conditions.AudienceRestrictions = nil },
		"another audience": func(a *saml.Assertion) {
			a.Conditions.AudienceRestrictions[0].Audience.Value = "https://other.example.test"
		},
		"second unrelated restriction": func(a *saml.Assertion) {
			a.Conditions.AudienceRestrictions = append(a.Conditions.AudienceRestrictions, saml.AudienceRestriction{Audience: saml.Audience{Value: "other"}})
		},
		"expired":               func(a *saml.Assertion) { a.Conditions.NotOnOrAfter = time.Now().Add(-time.Second) },
		"future":                func(a *saml.Assertion) { a.Conditions.NotBefore = time.Now().Add(5 * time.Minute) },
		"long validity":         func(a *saml.Assertion) { a.Conditions.NotOnOrAfter = time.Now().Add(time.Hour) },
		"stale authentication":  func(a *saml.Assertion) { a.AuthnStatements[0].AuthnInstant = time.Now().Add(-time.Hour) },
		"future authentication": func(a *saml.Assertion) { a.AuthnStatements[0].AuthnInstant = time.Now().Add(time.Hour) },
		"issuer":                func(a *saml.Assertion) { a.Issuer.Value = "https://wrong.example.test" },
		"transient subject":     func(a *saml.Assertion) { a.Subject.NameID.Format = string(saml.TransientNameIDFormat) },
		"empty subject":         func(a *saml.Assertion) { a.Subject.NameID.Value = "" },
		"qualifier":             func(a *saml.Assertion) { a.Subject.NameID.SPNameQualifier = "other" },
		"confirmation absent":   func(a *saml.Assertion) { a.Subject.SubjectConfirmations = nil },
		"recipient": func(a *saml.Assertion) {
			a.Subject.SubjectConfirmations[0].SubjectConfirmationData.Recipient = "https://other.example.test"
		},
		"unsolicited":  func(a *saml.Assertion) { a.Subject.SubjectConfirmations[0].SubjectConfirmationData.InResponseTo = "" },
		"non bearer":   func(a *saml.Assertion) { a.Subject.SubjectConfirmations[0].Method = "unknown" },
		"authn absent": func(a *saml.Assertion) { a.AuthnStatements = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := f.assertion()
			change(&a)
			raw := f.response(t, a, dsig.RSASHA256SignatureMethod)
			if _, err := NewSAML().Verify(context.Background(), f.settings, "request-one", raw, f.started); err == nil {
				t.Fatal("invalid signed claims accepted")
			}
		})
	}
}
func TestSAMLRejectsSignatureAndXMLAttacks(t *testing.T) {
	f := newSAMLFixture(t)
	valid := f.response(t, f.assertion(), dsig.RSASHA256SignatureMethod)
	cases := map[string][]byte{
		"unsigned":      f.response(t, f.assertion(), ""),
		"sha1":          f.response(t, f.assertion(), dsig.RSASHA1SignatureMethod),
		"tampered":      bytes.Replace(valid, []byte("immutable-subject"), []byte("attacker-subject"), 1),
		"DTD":           append([]byte(`<!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>`), valid...),
		"oversized":     bytes.Repeat([]byte("x"), maxSAMLXML+1),
		"duplicate id":  bytes.Replace(valid, []byte(`ID="response-one"`), []byte(`ID="assertion-one"`), 1),
		"trailing root": append(append([]byte{}, valid...), []byte("<extra/>")...),
		"nil subject":   []byte(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`),
	}
	doc := etree.NewDocument()
	_ = doc.ReadFromBytes(valid)
	root := doc.Root()
	assertion := samlOne(root, samlNS, "Assertion")
	root.AddChild(assertion.Copy())
	root.ChildElements()[len(root.ChildElements())-1].CreateAttr("ID", "another-assertion")
	cases["two assertions"], _ = doc.WriteToBytes()
	doc = etree.NewDocument()
	_ = doc.ReadFromBytes(valid)
	root = doc.Root()
	assertion = samlOne(root, samlNS, "Assertion")
	root.RemoveChild(assertion)
	root.CreateElement("wrapper").AddChild(assertion)
	cases["wrapping"], _ = doc.WriteToBytes()
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewSAML().Verify(context.Background(), f.settings, "request-one", raw, f.started)
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if err.Error() != errProvider.Error() {
				t.Fatal("protocol error leaked assertion details", err)
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSAML().Verify(canceled, f.settings, "request-one", valid, f.started); err != context.Canceled {
		t.Fatal(err)
	}
}
func TestSAMLRejectsInvalidPinnedMetadata(t *testing.T) {
	f := newSAMLFixture(t)
	for name, metadata := range map[string]string{
		"localhost":       strings.ReplaceAll(f.settings.Metadata, "https://idp.example.test/sso", "https://127.0.0.1/sso"),
		"insecure":        strings.ReplaceAll(f.settings.Metadata, "https://idp.example.test/sso", "http://idp.example.test/sso"),
		"external entity": `<!DOCTYPE test SYSTEM "https://example.test">` + f.settings.Metadata,
		"expired":         strings.Replace(f.settings.Metadata, `entityID=`, `validUntil="2000-01-01T00:00:00Z" entityID=`, 1),
		"aggregate":       `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata">` + f.settings.Metadata + `</EntitiesDescriptor>`,
	} {
		t.Run(name, func(t *testing.T) {
			s := f.settings
			s.Metadata = metadata
			if _, err := NewSAML().Validate(context.Background(), s); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}
func FuzzSAMLXML(f *testing.F) {
	f.Add([]byte(`<Response xmlns="urn:oasis:names:tc:SAML:2.0:protocol"><Assertion ID="a"/></Response>`))
	f.Add([]byte(`<!DOCTYPE a><a/>`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = samlXML(b) })
}
