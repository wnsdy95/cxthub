package federation

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	xrv "github.com/mattermost/xml-roundtrip-validator"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/russellhaering/goxmldsig/etreeutils"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	samlNS       = "urn:oasis:names:tc:SAML:2.0:assertion"
	samlProtocol = "urn:oasis:names:tc:SAML:2.0:protocol"
	samlMetadata = "urn:oasis:names:tc:SAML:2.0:metadata"
	dsigNS       = "http://www.w3.org/2000/09/xmldsig#"
	maxSAMLXML   = 256 << 10
)

// SAML supports the SP-initiated redirect/POST profile with one signed plaintext
// assertion and a persistent NameID. Unsupported profiles fail closed. The
// application owns durable request/assertion replay prevention and browser binding.
type SAML struct{}

func NewSAML() *SAML { return &SAML{} }

var _ outbound.SAMLProvider = (*SAML)(nil)

// Bound parsing before handing XML to the protocol/signature libraries. Reject
// DTDs, duplicate IDs and deep input; do not resolve external entities.
func samlXML(raw []byte) (*etree.Element, error) {
	if len(raw) == 0 || len(raw) > maxSAMLXML {
		return nil, errProvider
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, roots := 0, 0
	ids := map[string]bool{}
	for {
		t, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errProvider
		}
		switch v := t.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			if depth > 32 || roots != 1 {
				return nil, errProvider
			}
			attrs := map[xml.Name]bool{}
			for _, a := range v.Attr {
				if attrs[a.Name] {
					return nil, errProvider
				}
				attrs[a.Name] = true
				if strings.EqualFold(a.Name.Local, "id") {
					if a.Value == "" || ids[a.Value] {
						return nil, errProvider
					}
					ids[a.Value] = true
				}
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return nil, errProvider
		case xml.ProcInst:
			if v.Target != "xml" || depth != 0 || roots != 0 {
				return nil, errProvider
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return nil, errProvider
			}
		}
	}
	if roots != 1 || depth != 0 || xrv.Validate(bytes.NewReader(raw)) != nil {
		return nil, errProvider
	}
	doc := etree.NewDocument()
	if doc.ReadFromBytes(raw) != nil || doc.Root() == nil {
		return nil, errProvider
	}
	return doc.Root(), nil
}
func samlChildren(e *etree.Element, ns, name string) []*etree.Element {
	var out []*etree.Element
	if e == nil {
		return out
	}
	for _, c := range e.ChildElements() {
		if c.NamespaceURI() == ns && c.Tag == name {
			out = append(out, c)
		}
	}
	return out
}
func samlOne(e *etree.Element, ns, name string) *etree.Element {
	v := samlChildren(e, ns, name)
	if len(v) != 1 {
		return nil
	}
	return v[0]
}
func samlOnly(e *etree.Element, allowed map[string]bool) bool {
	if e == nil {
		return false
	}
	for _, c := range e.ChildElements() {
		if !allowed[c.NamespaceURI()+"|"+c.Tag] {
			return false
		}
	}
	return true
}
func samlAllowed(ns string, names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[ns+"|"+n] = true
	}
	return m
}

func samlIDP(raw string) (*saml.EntityDescriptor, error) {
	root, err := samlXML([]byte(raw))
	if err != nil || root.Tag != "EntityDescriptor" || root.NamespaceURI() != samlMetadata || len(samlChildren(root, samlMetadata, "IDPSSODescriptor")) != 1 {
		return nil, errProvider
	}
	var md saml.EntityDescriptor
	if xml.Unmarshal([]byte(raw), &md) != nil || len(md.IDPSSODescriptors) != 1 || md.EntityID == "" || len(md.EntityID) > 2048 {
		return nil, errProvider
	}
	u, err := url.Parse(md.EntityID)
	if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "urn") {
		return nil, errProvider
	}
	now := time.Now()
	d := &md.IDPSSODescriptors[0]
	if (!md.ValidUntil.IsZero() && !now.Before(md.ValidUntil)) || (d.ValidUntil != nil && !now.Before(*d.ValidUntil)) || !strings.Contains(" "+d.ProtocolSupportEnumeration+" ", " "+samlProtocol+" ") {
		return nil, errProvider
	}
	endpoints := 0
	for _, ep := range d.SingleSignOnServices {
		if ep.Binding == saml.HTTPRedirectBinding {
			if !providerURL(ep.Location) || ep.ResponseLocation != "" {
				return nil, errProvider
			}
			endpoints++
		}
	}
	if endpoints != 1 {
		return nil, errProvider
	}
	certs := 0
	for _, k := range d.KeyDescriptors {
		if k.Use != "" && k.Use != "signing" {
			continue
		}
		for _, c := range k.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.Data), ""))
			if err != nil {
				return nil, errProvider
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				return nil, errProvider
			}
			switch key := cert.PublicKey.(type) {
			case *rsa.PublicKey:
				if key.N.BitLen() < 2048 {
					return nil, errProvider
				}
			case *ecdsa.PublicKey:
				if key.Curve.Params().BitSize < 256 {
					return nil, errProvider
				}
			default:
				return nil, errProvider
			}
			certs++
		}
	}
	if certs == 0 || certs > 4 {
		return nil, errProvider
	}
	return &md, nil
}

func (*SAML) Keys(ctx context.Context) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return "", "", errProvider
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", errProvider
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "CXTHub SAML signing"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(3, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", errProvider
	}
	if err = ctx.Err(); err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil
}
func samlSP(s outbound.SAMLSettings, metadataOnly ...bool) (*saml.ServiceProvider, error) {
	md, err := samlIDP(s.Metadata)
	if err != nil {
		return nil, err
	}
	entity, err := url.Parse(s.EntityID)
	if err != nil || !entity.IsAbs() || entity.Fragment != "" {
		return nil, errProvider
	}
	acs, err := url.Parse(s.ACS)
	if err != nil || !providerURL(s.ACS) {
		return nil, errProvider
	}
	cb, rest := pem.Decode([]byte(s.Certificate))
	if cb == nil || cb.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errProvider
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	allowExpired := len(metadataOnly) > 0 && metadataOnly[0]
	if err != nil || (!allowExpired && (time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter))) {
		return nil, errProvider
	}
	kb, rest := pem.Decode([]byte(s.PrivateKey))
	if kb == nil || kb.Type != "RSA PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errProvider
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil || key.N.BitLen() < 2048 {
		return nil, errProvider
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errProvider
	}
	force := true
	return &saml.ServiceProvider{EntityID: s.EntityID, MetadataURL: *entity, AcsURL: *acs, IDPMetadata: md, Key: key, Certificate: cert, ForceAuthn: &force, AuthnNameIDFormat: saml.PersistentNameIDFormat, SignatureMethod: dsig.RSASHA256SignatureMethod, SignatureVerifier: strongSAMLSignature{}}, nil
}
func (*SAML) Validate(ctx context.Context, s outbound.SAMLSettings) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sp, err := samlSP(s)
	if err != nil {
		return "", err
	}
	return sp.IDPMetadata.EntityID, nil
}

// Configuration recovery may update expired IdP trust while the current SP
// signer also needs renewal. This does not authorize a login with expired keys.
func (*SAML) ValidateMetadata(ctx context.Context, s outbound.SAMLSettings) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sp, err := samlSP(s, true)
	if err != nil {
		return "", err
	}
	return sp.IDPMetadata.EntityID, nil
}
func (*SAML) Metadata(ctx context.Context, s outbound.SAMLSettings) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Expired current certificates must not prevent publishing a replacement.
	// Authorize/Verify still require the active signing certificate to be valid.
	sp, err := samlSP(s, true)
	if err != nil {
		return "", err
	}
	md := sp.Metadata()
	if len(s.AdditionalCertificates) > 1 {
		return "", errProvider
	}
	// This profile accepts signed plaintext assertions only. Do not advertise
	// encryption/SLO endpoints that the application does not implement.
	for i := range md.SPSSODescriptors {
		d := &md.SPSSODescriptors[i]
		var keys []saml.KeyDescriptor
		for _, k := range d.KeyDescriptors {
			if k.Use == "signing" {
				keys = append(keys, k)
			}
		}
		d.KeyDescriptors = keys
		for _, raw := range s.AdditionalCertificates {
			block, rest := pem.Decode([]byte(raw))
			if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
				return "", errProvider
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return "", errProvider
			}
			if bytes.Equal(cert.Raw, sp.Certificate.Raw) {
				return "", errProvider
			}
			d.KeyDescriptors = append(d.KeyDescriptors, saml.KeyDescriptor{Use: "signing", KeyInfo: saml.KeyInfo{X509Data: saml.X509Data{X509Certificates: []saml.X509Certificate{{Data: base64.StdEncoding.EncodeToString(cert.Raw)}}}}})
		}
		d.SingleLogoutServices = nil
	}
	b, err := xml.Marshal(md)
	if err != nil {
		return "", errProvider
	}
	return string(b), nil
}
func (*SAML) Authorize(ctx context.Context, s outbound.SAMLSettings, id, state string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if id == "" || len(id) > 128 || len(state) != 64 {
		return "", errProvider
	}
	sp, err := samlSP(s)
	if err != nil {
		return "", err
	}
	req, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", errProvider
	}
	req.ID = id
	u, err := req.Redirect(state, sp)
	if err != nil {
		return "", errProvider
	}
	return u.String(), nil
}

type strongSAMLSignature struct{}

func (strongSAMLSignature) VerifySignature(v *dsig.ValidationContext, e *etree.Element) error {
	sig := samlOne(e, dsigNS, "Signature")
	si := samlOne(sig, dsigNS, "SignedInfo")
	method := samlOne(si, dsigNS, "SignatureMethod")
	ref := samlOne(si, dsigNS, "Reference")
	digest := samlOne(ref, dsigNS, "DigestMethod")
	if method == nil || digest == nil || ref.SelectAttrValue("URI", "") != "#"+e.SelectAttrValue("ID", "") {
		return errProvider
	}
	switch method.SelectAttrValue("Algorithm", "") {
	case dsig.RSASHA256SignatureMethod, dsig.RSASHA384SignatureMethod, dsig.RSASHA512SignatureMethod, dsig.ECDSASHA256SignatureMethod, dsig.ECDSASHA384SignatureMethod, dsig.ECDSASHA512SignatureMethod:
	default:
		return errProvider
	}
	switch digest.SelectAttrValue("Algorithm", "") {
	case "http://www.w3.org/2001/04/xmlenc#sha256", "http://www.w3.org/2001/04/xmldsig-more#sha384", "http://www.w3.org/2001/04/xmlenc#sha512":
	default:
		return errProvider
	}
	_, err := v.Validate(e)
	if err != nil {
		return errProvider
	}
	return nil
}

func (*SAML) Verify(ctx context.Context, s outbound.SAMLSettings, id string, raw []byte, started time.Time) (outbound.SAMLProof, error) {
	var zero outbound.SAMLProof
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	sp, err := samlSP(s)
	if err != nil {
		return zero, err
	}
	root, err := samlXML(raw)
	if err != nil || root.Tag != "Response" || root.NamespaceURI() != samlProtocol {
		return zero, errProvider
	}
	allowed := samlAllowed(samlProtocol, "Status")
	allowed[samlNS+"|Issuer"] = true
	allowed[samlNS+"|Assertion"] = true
	allowed[dsigNS+"|Signature"] = true
	if !samlOnly(root, allowed) || samlOne(root, samlNS, "Issuer") == nil || samlOne(root, samlProtocol, "Status") == nil || len(samlChildren(root, dsigNS, "Signature")) > 1 || root.SelectAttrValue("Version", "") != "2.0" || root.SelectAttrValue("ID", "") == "" || root.SelectAttrValue("Destination", "") != s.ACS || root.SelectAttrValue("InResponseTo", "") != id || id == "" {
		return zero, errProvider
	}
	ael := samlOne(root, samlNS, "Assertion")
	sub := samlOne(ael, samlNS, "Subject")
	cond := samlOne(ael, samlNS, "Conditions")
	confirmation := samlOne(sub, samlNS, "SubjectConfirmation")
	if ael == nil || samlOne(ael, dsigNS, "Signature") == nil || samlOne(ael, samlNS, "Issuer") == nil || samlOne(sub, samlNS, "NameID") == nil || samlOne(confirmation, samlNS, "SubjectConfirmationData") == nil || samlOne(ael, samlNS, "AuthnStatement") == nil || cond == nil {
		return zero, errProvider
	}
	allowed = samlAllowed(samlNS, "Issuer", "Subject", "Conditions", "AuthnStatement", "AttributeStatement")
	allowed[dsigNS+"|Signature"] = true
	if !samlOnly(ael, allowed) || !samlOnly(sub, samlAllowed(samlNS, "NameID", "SubjectConfirmation")) || !samlOnly(cond, samlAllowed(samlNS, "AudienceRestriction", "OneTimeUse")) {
		return zero, errProvider
	}
	ars := samlChildren(cond, samlNS, "AudienceRestriction")
	if len(ars) == 0 {
		return zero, errProvider
	}
	for _, ar := range ars {
		au := samlOne(ar, samlNS, "Audience")
		if au == nil || au.Text() != s.EntityID || !samlOnly(ar, samlAllowed(samlNS, "Audience")) {
			return zero, errProvider
		}
	}
	// Validate assertion signature even if a signed response would permit the
	// library to skip it. This profile always requires both signatures to verify
	// when both are present, and never accepts an unsigned assertion.
	a, err := sp.ParseXMLResponse(raw, []string{id}, sp.AcsURL)
	if err != nil {
		return zero, errProvider
	}
	// Verify the assertion independently with the pinned metadata roots.
	if err = verifySAMLAssertionSignature(sp, ael); err != nil {
		return zero, errProvider
	}
	if a.ID == "" || a.Version != "2.0" || a.Subject == nil || a.Subject.NameID == nil || a.Conditions == nil || len(a.Subject.SubjectConfirmations) != 1 || len(a.AuthnStatements) != 1 {
		return zero, errProvider
	}
	n := a.Subject.NameID
	c := a.Subject.SubjectConfirmations[0]
	d := c.SubjectConfirmationData
	now := time.Now()
	auth := a.AuthnStatements[0]
	if n.Format != string(saml.PersistentNameIDFormat) || n.Value == "" || len(n.Value) > 1024 || (n.NameQualifier != "" && n.NameQualifier != sp.IDPMetadata.EntityID) || (n.SPNameQualifier != "" && n.SPNameQualifier != s.EntityID) || n.SPProvidedID != "" || c.Method != "urn:oasis:names:tc:SAML:2.0:cm:bearer" || d == nil || d.InResponseTo != id || d.Recipient != s.ACS || !d.NotBefore.IsZero() || c.NameID != nil {
		return zero, errProvider
	}
	var response saml.Response
	if xml.Unmarshal(raw, &response) != nil {
		return zero, errProvider
	}
	if response.Issuer == nil || response.Issuer.Value != sp.IDPMetadata.EntityID || response.IssueInstant.After(now.Add(time.Minute)) || a.IssueInstant.After(now.Add(time.Minute)) || a.IssueInstant.Before(started.Add(-time.Minute)) || auth.AuthnInstant.Before(started.Add(-time.Minute)) || auth.AuthnInstant.After(now.Add(time.Minute)) || started.IsZero() || started.After(now) || now.Sub(started) > 10*time.Minute {
		return zero, errProvider
	}
	expires := a.Conditions.NotOnOrAfter
	if a.Conditions.NotBefore.After(now.Add(time.Minute)) || expires.IsZero() || !now.Before(expires) || expires.After(now.Add(10*time.Minute)) || !now.Before(d.NotOnOrAfter) {
		return zero, errProvider
	}
	if d.NotOnOrAfter.Before(expires) {
		expires = d.NotOnOrAfter
	}
	if auth.SessionNotOnOrAfter != nil && auth.SessionNotOnOrAfter.Before(expires) {
		expires = *auth.SessionNotOnOrAfter
	}
	if !now.Before(expires) {
		return zero, errProvider
	}
	acr := ""
	if auth.AuthnContext.AuthnContextClassRef != nil {
		acr = auth.AuthnContext.AuthnContextClassRef.Value
	}
	if len(acr) > 2048 {
		return zero, errProvider
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	return outbound.SAMLProof{AssertionID: a.ID, FederationProof: outbound.FederationProof{Issuer: a.Issuer.Value, Subject: n.Value, AuthenticatedAt: auth.AuthnInstant, ExpiresAt: expires, ACR: acr}}, nil
}
func verifySAMLAssertionSignature(sp *saml.ServiceProvider, el *etree.Element) error {
	// Preserve inherited namespace context exactly as the protocol library does.
	// Validating a detached subtree without those bindings can change C14N bytes.
	ns, err := etreeutils.NSBuildParentContext(el)
	if err != nil {
		return errProvider
	}
	ns, err = ns.SubContext(el)
	if err != nil {
		return errProvider
	}
	el, err = etreeutils.NSDetatch(ns, el)
	if err != nil {
		return errProvider
	}
	var certs []*x509.Certificate
	for _, k := range sp.IDPMetadata.IDPSSODescriptors[0].KeyDescriptors {
		if k.Use != "" && k.Use != "signing" {
			continue
		}
		for _, c := range k.KeyInfo.X509Data.X509Certificates {
			b, _ := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.Data), ""))
			cert, err := x509.ParseCertificate(b)
			if err != nil {
				return errProvider
			}
			certs = append(certs, cert)
		}
	}
	v := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: certs})
	v.IdAttribute = "ID"
	return (strongSAMLSignature{}).VerifySignature(v, el)
}
