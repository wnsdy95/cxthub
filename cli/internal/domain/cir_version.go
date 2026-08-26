package domain

import "fmt"

const (
	CIRVersionV1 = "1"
	CIRVersionV2 = "2"
)

// SupportedCIRVersions returns a fresh capability list for sync negotiation.
func SupportedCIRVersions() []string {
	return []string{CIRVersionV1, CIRVersionV2}
}

// SupportsCIRVersion reports whether versions explicitly advertises version.
func SupportsCIRVersion(versions []string, version string) bool {
	for _, candidate := range versions {
		if candidate == version {
			return true
		}
	}
	return false
}

// CIRVersionForEvents chooses the oldest CIR version that can represent events.
func CIRVersionForEvents(events []Event) string {
	if eventsRequireCIRV2(events) {
		return CIRVersionV2
	}
	return CIRVersionV1
}

// ValidateCIRVersion prevents a v2 event shape from being mislabeled as v1.
// An empty version remains readable for pre-schema local fixtures and legacy
// objects, but it has the same feature ceiling as v1.
func ValidateCIRVersion(doc CIRDocument) error {
	switch doc.Envelope.CIRVersion {
	case "", CIRVersionV1:
		if eventsRequireCIRV2(doc.Events) {
			return fmt.Errorf("%w: CIR v2 event fields require cir_version %q", ErrInvalidCIR, CIRVersionV2)
		}
		return nil
	case CIRVersionV2:
		return nil
	default:
		return fmt.Errorf("%w: CIR version %q", ErrUnsupportedCIRVersion, doc.Envelope.CIRVersion)
	}
}

func eventsRequireCIRV2(events []Event) bool {
	for _, event := range events {
		if event.Kind == EventCompaction ||
			event.AgentMessage || event.AgentAuthor != "" || event.AgentRecipient != "" || event.ProviderMetadata != nil ||
			(event.Kind == EventMessage && event.Locked != nil) ||
			event.Replacement != nil || event.ReplacementComplete ||
			event.fieldPresence&eventV2PresenceMask != 0 ||
			(event.Kind == EventMessage && event.fieldPresence&eventFieldLocked != 0) {
			return true
		}
		if eventsRequireCIRV2(event.Replacement) {
			return true
		}
	}
	return false
}
