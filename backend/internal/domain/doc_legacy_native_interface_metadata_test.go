package domain

import ()

type legacyNativeInterfaceMetadataString string

func (value legacyNativeInterfaceMetadataString) String() string {
	return string(value)
}
