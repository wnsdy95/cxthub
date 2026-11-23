package domain

import (
	"encoding/json"
)

type legacyNativeMetadataJSONAlias string

func (value *legacyNativeMetadataJSONAlias) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*value = legacyNativeMetadataJSONAlias("json:" + decoded)
	return nil
}

type legacyNativeMetadataTextAlias string

func (value *legacyNativeMetadataTextAlias) UnmarshalText(raw []byte) error {
	*value = legacyNativeMetadataTextAlias("text:" + string(raw))
	return nil
}
