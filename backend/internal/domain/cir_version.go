package domain

import "fmt"

const (
	CIRVersionV1 = "1"
	CIRVersionV2 = "2"
)

func SupportedCIRVersions() []string {
	return []string{CIRVersionV1, CIRVersionV2}
}

func SupportsCIRVersion(versions []string, version string) bool {
	for _, candidate := range versions {
		if candidate == version {
			return true
		}
	}
	return false
}

// ValidateCIRVersion keeps the content-addressed object honest: v2-only event
// fields cannot travel under a v1 envelope during a rolling upgrade.
func ValidateCIRVersion(doc CIRDocument) error {
	switch doc.Envelope.CIRVersion {
	case "", CIRVersionV1:
		if eventsRequireCIRV2(doc.Events) {
			return fmt.Errorf("%w: CIR v2 event fields require cir_version %q", ErrUnsupportedCIRVersion, CIRVersionV2)
		}
		return nil
	case CIRVersionV2:
		return nil
	default:
		return fmt.Errorf("%w: CIR version %q", ErrUnsupportedCIRVersion, doc.Envelope.CIRVersion)
	}
}

func eventsRequireCIRV2(events []CIREvent) bool {
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
