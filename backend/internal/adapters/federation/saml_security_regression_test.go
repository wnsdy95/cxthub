package federation

import (
	"bytes"
	"context"
	"crypto/x509"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/russellhaering/goxmldsig/etreeutils"
)

func (fixture samlFixture) signedModifiedResponse(t *testing.T, change func(*etree.Element)) []byte {
	t.Helper()
	document := etree.NewDocument()
	if err := document.ReadFromBytes(fixture.response(t, fixture.assertion(), "")); err != nil {
		t.Fatal(err)
	}
	root := document.Root()
	change(root)
	assertion := samlOne(root, samlNS, "Assertion")
	namespaces, err := etreeutils.NSBuildParentContext(assertion)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, err = namespaces.SubContext(assertion)
	if err != nil {
		t.Fatal(err)
	}
	detached, err := etreeutils.NSDetatch(namespaces, assertion)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := dsig.NewSigningContext(fixture.key, [][]byte{fixture.cert.Raw})
	if err != nil {
		t.Fatal(err)
	}
	signer.IdAttribute = "ID"
	signer.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err = signer.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		t.Fatal(err)
	}
	signed, err := signer.SignEnveloped(detached)
	if err != nil {
		t.Fatal(err)
	}
	root.RemoveChild(assertion)
	root.AddChild(signed)
	raw, err := document.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := samlXML(raw)
	if err != nil {
		t.Fatal("signed fixture is not well-formed", err)
	}
	assertion = samlOne(parsed, samlNS, "Assertion")
	namespaces, err = etreeutils.NSBuildParentContext(assertion)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, err = namespaces.SubContext(assertion)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err = etreeutils.NSDetatch(namespaces, assertion)
	if err != nil {
		t.Fatal(err)
	}
	validator := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{fixture.cert}})
	validator.IdAttribute = "ID"
	if _, err = validator.Validate(assertion); err != nil {
		t.Fatal("fixture must have a valid assertion signature", err)
	}
	return raw
}

func TestSAMLInboundClaimsAndFreshness(t *testing.T) {
	fixture := newSAMLFixture(t)
	fixture.started = time.Now().Add(-6 * time.Minute)
	cases := map[string]func(*etree.Element){
		"unsuccessful status": func(root *etree.Element) {
			samlOne(samlOne(root, samlProtocol, "Status"), samlProtocol, "StatusCode").CreateAttr("Value", "urn:oasis:names:tc:SAML:2.0:status:Responder")
		},
		"missing status code": func(root *etree.Element) {
			status := samlOne(root, samlProtocol, "Status")
			status.RemoveChild(samlOne(status, samlProtocol, "StatusCode"))
		},
		"foreign status code": func(root *etree.Element) {
			code := samlOne(samlOne(root, samlProtocol, "Status"), samlProtocol, "StatusCode")
			code.Space = "foreign"
			code.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
		},
		"duplicate status code": func(root *etree.Element) {
			status := samlOne(root, samlProtocol, "Status")
			status.AddChild(samlOne(status, samlProtocol, "StatusCode").Copy())
		},
		"wrong assertion issuer": func(root *etree.Element) {
			samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Issuer").SetText("https://other.example.test")
		},
		"old response": func(root *etree.Element) {
			root.CreateAttr("IssueInstant", time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339Nano))
		},
		"old assertion": func(root *etree.Element) {
			samlOne(root, samlNS, "Assertion").CreateAttr("IssueInstant", time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339Nano))
		},
		"missing response time":    func(root *etree.Element) { root.RemoveAttr("IssueInstant") },
		"missing assertion time":   func(root *etree.Element) { samlOne(root, samlNS, "Assertion").RemoveAttr("IssueInstant") },
		"malformed assertion time": func(root *etree.Element) { samlOne(root, samlNS, "Assertion").CreateAttr("IssueInstant", "not-a-time") },
		"foreign confirmation data": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			data := samlOne(samlOne(subject, samlNS, "SubjectConfirmation"), samlNS, "SubjectConfirmationData")
			data.Space = "foreign"
			data.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
		},
		"foreign authentication context": func(root *etree.Element) {
			authentication := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "AuthnStatement")
			context := samlOne(authentication, samlNS, "AuthnContext")
			context.Space = "foreign"
			context.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
		},
		"nested identity": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			samlOne(subject, samlNS, "NameID").CreateElement("nested").SetText("attacker")
		},
		"nested audience": func(root *etree.Element) {
			conditions := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Conditions")
			samlOne(samlOne(conditions, samlNS, "AudienceRestriction"), samlNS, "Audience").CreateElement("nested").SetText("other")
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			raw := fixture.signedModifiedResponse(t, change)
			if proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started); err != errProvider || proof.Subject != "" {
				t.Fatalf("invalid claims accepted or unexpected error: %v", err)
			}
		})
	}
	for _, prefixFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "prefix first", false: "prefix last"}[prefixFirst], func(t *testing.T) {
			raw := fixture.signedModifiedResponse(t, func(root *etree.Element) {
				nameID := samlOne(samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject"), samlNS, "NameID")
				format := nameID.SelectAttrValue("Format", "")
				nameID.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
				nameID.RemoveAttr("Format")
				if prefixFirst {
					nameID.CreateAttr("foreign:Format", format)
				}
				nameID.CreateAttr("Format", format)
				if !prefixFirst {
					nameID.CreateAttr("foreign:Format", format)
				}
			})
			if _, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started); err != errProvider {
				t.Fatalf("ambiguous attributes accepted: %v", err)
			}
		})
	}
}

func TestSAMLIssueInstantBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		instant time.Time
		valid   bool
	}{
		{now.Add(-90 * time.Second), true},
		{now.Add(-90*time.Second - time.Nanosecond), false},
		{now.Add(time.Minute), true},
		{now.Add(time.Minute + time.Nanosecond), false},
		{time.Time{}, false},
	} {
		if validSAMLIssueInstant(test.instant, now) != test.valid {
			t.Fatalf("unexpected freshness for %v", test.instant)
		}
	}
}

func TestSAMLRequiresEveryPresentSignature(t *testing.T) {
	fixture := newSAMLFixture(t)
	valid := fixture.signedModifiedResponse(t, func(*etree.Element) {})
	for _, test := range []struct {
		name         string
		alter        func(*etree.Element)
		signResponse bool
		valid        bool
	}{
		{"assertion only", func(*etree.Element) {}, false, true},
		{"both signatures", func(*etree.Element) {}, true, true},
		{"invalid assertion despite valid response", func(root *etree.Element) {
			samlOne(samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject"), samlNS, "NameID").SetText("attacker-subject")
		}, true, false},
		{"response only", func(root *etree.Element) {
			assertion := samlOne(root, samlNS, "Assertion")
			assertion.RemoveChild(samlOne(assertion, dsigNS, "Signature"))
		}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := etree.NewDocument()
			if err := document.ReadFromBytes(valid); err != nil {
				t.Fatal(err)
			}
			test.alter(document.Root())
			if test.signResponse {
				signer, err := dsig.NewSigningContext(fixture.key, [][]byte{fixture.cert.Raw})
				if err != nil {
					t.Fatal(err)
				}
				signer.IdAttribute = "ID"
				signer.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
				if err = signer.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
					t.Fatal(err)
				}
				signed, err := signer.SignEnveloped(document.Root())
				if err != nil {
					t.Fatal(err)
				}
				document.SetRoot(signed)
			}
			raw, err := document.WriteToBytes()
			if err != nil {
				t.Fatal(err)
			}
			proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
			if test.valid {
				if err != nil || proof.Subject != "immutable-subject" {
					t.Fatalf("valid signatures rejected: %v", err)
				}
			} else if err != errProvider || proof.Subject != "" {
				t.Fatalf("invalid signatures accepted: %v", err)
			}
			if test.signResponse && test.valid {
				tampered := bytes.Replace(raw, []byte(`ID="response-one"`), []byte(`ID="response-modified"`), 1)
				if _, err = NewSAML().Verify(context.Background(), fixture.settings, "request-one", tampered, fixture.started); err != errProvider {
					t.Fatalf("invalid response signature accepted: %v", err)
				}
			}
		})
	}
}

func TestSAMLValidNamespaceAndScalarForms(t *testing.T) {
	fixture := newSAMLFixture(t)
	for name, change := range map[string]func(*etree.Element){
		"alternate prefix inherited from response": func(root *etree.Element) {
			root.CreateAttr("xmlns:identity", samlNS)
			var rename func(*etree.Element)
			rename = func(element *etree.Element) {
				if element.NamespaceURI() == samlNS {
					element.Space = "identity"
				}
				for _, child := range element.ChildElements() {
					rename(child)
				}
			}
			rename(root)
		},
		"default assertion namespace": func(root *etree.Element) {
			root.CreateAttr("xmlns", samlNS)
			var rename func(*etree.Element)
			rename = func(element *etree.Element) {
				if element.NamespaceURI() == samlNS {
					element.Space = ""
				}
				for _, child := range element.ChildElements() {
					rename(child)
				}
			}
			rename(root)
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw := fixture.signedModifiedResponse(t, change)
			proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
			if err != nil || proof.Subject != "immutable-subject" || proof.Issuer != "https://idp.example.test" {
				t.Fatalf("valid claim representation rejected or changed: %v", err)
			}
		})
	}
	t.Run("comment and cdata in identity", func(t *testing.T) {
		raw := fixture.signedModifiedResponse(t, func(*etree.Element) {})
		raw = bytes.Replace(raw, []byte(">immutable-subject<"), []byte(">immutable-<!--not identity data--><![CDATA[subject]]><"), 1)
		proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
		if err != nil || proof.Subject != "immutable-subject" {
			t.Fatalf("equivalent signed identity changed: %v", err)
		}
	})
	other := newSAMLFixture(t)
	raw := other.response(t, other.assertion(), dsig.RSASHA256SignatureMethod)
	if _, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started); err != errProvider {
		t.Fatalf("unpinned signing key accepted: %v", err)
	}
}

func TestSAMLDualSignaturesWithDifferentCanonicalization(t *testing.T) {
	fixture := newSAMLFixture(t)
	document := etree.NewDocument()
	if err := document.ReadFromBytes(fixture.response(t, fixture.assertion(), "")); err != nil {
		t.Fatal(err)
	}
	root := document.Root()
	root.CreateAttr("xmlns:retained", "urn:cxthub:test:retained")
	assertion := samlOne(root, samlNS, "Assertion")
	sign := func(element *etree.Element, canonicalizer dsig.Canonicalizer) *etree.Element {
		t.Helper()
		namespaces, err := etreeutils.NSBuildParentContext(element)
		if err != nil {
			t.Fatal(err)
		}
		namespaces, err = namespaces.SubContext(element)
		if err != nil {
			t.Fatal(err)
		}
		element, err = etreeutils.NSDetatch(namespaces, element)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := dsig.NewSigningContext(fixture.key, [][]byte{fixture.cert.Raw})
		if err != nil {
			t.Fatal(err)
		}
		signer.IdAttribute = "ID"
		signer.Canonicalizer = canonicalizer
		if err = signer.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
			t.Fatal(err)
		}
		signature, err := signer.ConstructSignature(element.Copy(), true)
		if err != nil {
			t.Fatal(err)
		}
		element.AddChild(signature)
		return element
	}
	signedAssertion := sign(assertion, dsig.MakeC14N11Canonicalizer())
	root.RemoveChild(assertion)
	root.AddChild(signedAssertion)
	document.SetRoot(sign(root, dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")))
	raw, err := document.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := samlSP(fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := samlXML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifiedSAMLElement(provider, samlOne(parsed, samlNS, "Assertion")); err != nil {
		t.Fatal("fixture assertion signature is invalid", err)
	}
	if _, err = verifiedSAMLElement(provider, parsed); err != nil {
		t.Fatal("fixture response signature is invalid", err)
	}
	proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
	if err != nil || proof.Subject != "immutable-subject" {
		t.Fatalf("independently valid signatures rejected: %v", err)
	}
}

func TestSAMLRejectsSignedGO20266909Inputs(t *testing.T) {
	fixture := newSAMLFixture(t)
	cases := map[string]func(*etree.Element){
		"missing issuer": func(root *etree.Element) {
			assertion := samlOne(root, samlNS, "Assertion")
			assertion.RemoveChild(samlOne(assertion, samlNS, "Issuer"))
		},
		"missing subject": func(root *etree.Element) {
			assertion := samlOne(root, samlNS, "Assertion")
			assertion.RemoveChild(samlOne(assertion, samlNS, "Subject"))
		},
		"missing conditions": func(root *etree.Element) {
			assertion := samlOne(root, samlNS, "Assertion")
			assertion.RemoveChild(samlOne(assertion, samlNS, "Conditions"))
		},
		"missing confirmation data": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			confirmation := samlOne(subject, samlNS, "SubjectConfirmation")
			confirmation.RemoveChild(samlOne(confirmation, samlNS, "SubjectConfirmationData"))
		},
		"foreign namespace name id": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			nameID := samlOne(subject, samlNS, "NameID")
			nameID.Space = "foreign"
			nameID.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
		},
		"foreign name id after real name id": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			nameID := samlOne(subject, samlNS, "NameID").Copy()
			nameID.Space = "foreign"
			nameID.CreateAttr("xmlns:foreign", "urn:cxthub:test:foreign")
			nameID.SetText("attacker-subject")
			subject.AddChild(nameID)
		},
		"duplicate real name id": func(root *etree.Element) {
			subject := samlOne(samlOne(root, samlNS, "Assertion"), samlNS, "Subject")
			nameID := samlOne(subject, samlNS, "NameID").Copy()
			nameID.SetText("attacker-subject")
			subject.AddChild(nameID)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			raw := fixture.signedModifiedResponse(t, change)
			proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
			if err != errProvider || proof.Subject != "" || proof.AssertionID != "" {
				t.Fatalf("invalid signed response returned a proof or unexpected error: %v", err)
			}
		})
	}
	t.Run("valid signed control", func(t *testing.T) {
		raw := fixture.signedModifiedResponse(t, func(*etree.Element) {})
		proof, err := NewSAML().Verify(context.Background(), fixture.settings, "request-one", raw, fixture.started)
		if err != nil || proof.Subject != "immutable-subject" {
			t.Fatalf("valid signed response rejected: %v", err)
		}
	})
}
