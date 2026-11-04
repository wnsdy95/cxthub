package documentquery

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func readOnlyConstructorConfig(test *testing.T, profile Profile) Config {
	test.Helper()
	config := constructorPrefixFullConfig(test, profile)
	config.ReadOnly = true
	config.View.VisibleReadiness = config.View.VisiblePreparation
	config.View.BoundaryRead = config.View.Boundaries
	config.View.ByteReadiness = config.View.Bytes
	config.View.RangeJobs = nil
	config.View.LegacyPreparation = nil
	config.View.VisiblePreparation = nil
	config.View.Boundaries = nil
	config.View.Bytes = nil
	config.Find.FieldsRead = config.Find.Fields
	config.Find.Fields = nil
	config.Find.Display = nil
	return config
}

func TestReadOnlyConstructorAcceptsExplicitReadPortsWithoutDemand(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, VerifiedRoots} {
		config := readOnlyConstructorConfig(test, profile)
		queries := constructorPrefixFullAssemble(test, config)
		if !queries.readOnly || queries.view.VisibleReadiness != config.View.VisibleReadiness || queries.view.BoundaryRead != config.View.BoundaryRead ||
			queries.view.ByteReadiness != config.View.ByteReadiness || queries.find.FieldsRead != config.Find.FieldsRead {
			test.Fatal("read-only constructor dropped policy or replaced explicit read ports")
		}
		config.ReadOnly = false
		if !queries.readOnly {
			test.Fatal("caller changed a constructed read-only policy")
		}
	}
	config := constructorPrefixFullConfig(test, LegacyCompatibility)
	queries := constructorPrefixFullAssemble(test, config)
	if queries.readOnly || queries.view.RangeJobs != config.View.RangeJobs || queries.view.VisiblePreparation != config.View.VisiblePreparation ||
		queries.view.Boundaries != config.View.Boundaries || queries.view.Bytes != config.View.Bytes || queries.find.Fields != config.Find.Fields || queries.find.Display != config.Find.Display {
		test.Fatal("default policy changed existing mixed-port command wiring")
	}
}

func TestReadOnlyConstructorRejectsEveryLocalDemandBeforePortCalls(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, VerifiedRoots} {
		for _, name := range []string{"LegacyPreparation", "RangeJobs", "VisiblePreparation", "Boundaries", "Bytes", "BoundaryPreparation", "BytePreparation"} {
			test.Run(name, func(test *testing.T) {
				for _, typedNil := range []bool{false, true} {
					config := readOnlyConstructorConfig(test, profile)
					port := config.Legacy.(*constructorPrefixPorts)
					switch name {
					case "LegacyPreparation":
						config.View.LegacyPreparation = port
					case "RangeJobs":
						config.View.RangeJobs = port
					case "VisiblePreparation":
						config.View.VisiblePreparation = port
					case "Boundaries":
						config.View.Boundaries = port
					case "Bytes":
						config.View.Bytes = port
					case "BoundaryPreparation":
						config.View.BoundaryPreparation = port
					case "BytePreparation":
						config.View.BytePreparation = port
					}
					if typedNil {
						constructorPrefixFullClearField(test, config.View, name, true)
					}
					constructorPrefixAssertRejected(test, config, domain.ErrValidation)
				}
			})
		}
		for _, name := range []string{"Fields", "FieldsDemand", "Display"} {
			test.Run(name, func(test *testing.T) {
				for _, typedNil := range []bool{false, true} {
					config := readOnlyConstructorConfig(test, profile)
					port := config.Legacy.(*constructorPrefixPorts)
					switch name {
					case "Fields":
						config.Find.Fields = port
					case "FieldsDemand":
						config.Find.FieldsDemand = port
					case "Display":
						config.Find.Display = port
					}
					if typedNil {
						constructorPrefixFullClearField(test, config.Find, name, true)
					}
					constructorPrefixAssertRejected(test, config, domain.ErrValidation)
				}
			})
		}
	}
}

func TestReadOnlyConstructorValidatesReadinessRatherThanWriterPresence(test *testing.T) {
	for _, name := range []string{"VisibleReadiness", "BoundaryRead", "ByteReadiness"} {
		config := readOnlyConstructorConfig(test, VerifiedRoots)
		constructorPrefixFullClearField(test, config.View, name, true)
		constructorPrefixAssertRejected(test, config, domain.ErrValidation)
		config = readOnlyConstructorConfig(test, VerifiedRoots)
		constructorPrefixFullClearField(test, config.View, name, false)
		if name == "ByteReadiness" {
			constructorPrefixFullAssemble(test, config)
		} else {
			constructorPrefixAssertRejected(test, config, domain.ErrUnsupportedDocumentIdentity)
		}
	}
	config := readOnlyConstructorConfig(test, VerifiedRoots)
	constructorPrefixFullClearField(test, config.Find, "FieldsRead", true)
	constructorPrefixAssertRejected(test, config, domain.ErrValidation)
	config = readOnlyConstructorConfig(test, VerifiedRoots)
	config.Find.FieldsRead = nil
	constructorPrefixFullAssemble(test, config)
}
