package federation

import (
	"encoding/xml"
	"time"

	"github.com/beevik/etree"
)

type samlResponseClaims struct {
	XMLName      xml.Name  `xml:"urn:oasis:names:tc:SAML:2.0:protocol Response"`
	IssueInstant time.Time `xml:"IssueInstant,attr"`
	Issuer       *samlText `xml:"urn:oasis:names:tc:SAML:2.0:assertion Issuer"`
}

type samlAssertionClaims struct {
	XMLName         xml.Name             `xml:"urn:oasis:names:tc:SAML:2.0:assertion Assertion"`
	ID              string               `xml:"ID,attr"`
	Version         string               `xml:"Version,attr"`
	IssueInstant    time.Time            `xml:"IssueInstant,attr"`
	Issuer          *samlText            `xml:"urn:oasis:names:tc:SAML:2.0:assertion Issuer"`
	Subject         *samlSubject         `xml:"urn:oasis:names:tc:SAML:2.0:assertion Subject"`
	Conditions      *samlConditions      `xml:"urn:oasis:names:tc:SAML:2.0:assertion Conditions"`
	AuthnStatements []samlAuthentication `xml:"urn:oasis:names:tc:SAML:2.0:assertion AuthnStatement"`
}

type samlText struct {
	Value string `xml:",chardata"`
}

type samlSubject struct {
	NameID               *samlNameID        `xml:"urn:oasis:names:tc:SAML:2.0:assertion NameID"`
	SubjectConfirmations []samlConfirmation `xml:"urn:oasis:names:tc:SAML:2.0:assertion SubjectConfirmation"`
}

type samlNameID struct {
	Value           string `xml:",chardata"`
	Format          string `xml:"Format,attr"`
	NameQualifier   string `xml:"NameQualifier,attr"`
	SPNameQualifier string `xml:"SPNameQualifier,attr"`
	SPProvidedID    string `xml:"SPProvidedID,attr"`
}

type samlConfirmation struct {
	Method                  string                `xml:"Method,attr"`
	NameID                  *samlNameID           `xml:"urn:oasis:names:tc:SAML:2.0:assertion NameID"`
	SubjectConfirmationData *samlConfirmationData `xml:"urn:oasis:names:tc:SAML:2.0:assertion SubjectConfirmationData"`
}

type samlConfirmationData struct {
	InResponseTo string    `xml:"InResponseTo,attr"`
	Recipient    string    `xml:"Recipient,attr"`
	NotBefore    time.Time `xml:"NotBefore,attr"`
	NotOnOrAfter time.Time `xml:"NotOnOrAfter,attr"`
}

type samlConditions struct {
	NotBefore    time.Time `xml:"NotBefore,attr"`
	NotOnOrAfter time.Time `xml:"NotOnOrAfter,attr"`
}

type samlAuthentication struct {
	AuthnInstant        time.Time  `xml:"AuthnInstant,attr"`
	SessionNotOnOrAfter *time.Time `xml:"SessionNotOnOrAfter,attr"`
	AuthnContext        struct {
		AuthnContextClassRef *samlText `xml:"urn:oasis:names:tc:SAML:2.0:assertion AuthnContextClassRef"`
	} `xml:"urn:oasis:names:tc:SAML:2.0:assertion AuthnContext"`
}

func samlScalar(element *etree.Element) bool {
	return element != nil && len(element.ChildElements()) == 0
}

func samlUnqualifiedClaims(element *etree.Element) bool {
	for _, attribute := range element.Attr {
		if attribute.Space == "" || attribute.Space == "xmlns" {
			continue
		}
		switch attribute.Key {
		case "ID", "Version", "Destination", "InResponseTo", "IssueInstant", "Value", "Format", "NameQualifier", "SPNameQualifier", "SPProvidedID", "Method", "Recipient", "NotBefore", "NotOnOrAfter", "AuthnInstant", "SessionNotOnOrAfter", "URI", "Algorithm":
			return false
		}
	}
	for _, child := range element.ChildElements() {
		if !samlUnqualifiedClaims(child) {
			return false
		}
	}
	return true
}

func samlDecodeClaims(element *etree.Element, claims any) error {
	document := etree.NewDocument()
	document.SetRoot(element.Copy())
	raw, err := document.WriteToBytes()
	if err != nil || xml.Unmarshal(raw, claims) != nil {
		return errProvider
	}
	return nil
}

func validSAMLIssueInstant(instant, now time.Time) bool {
	return !instant.IsZero() && !instant.Add(90*time.Second).Before(now) && !instant.After(now.Add(time.Minute))
}
