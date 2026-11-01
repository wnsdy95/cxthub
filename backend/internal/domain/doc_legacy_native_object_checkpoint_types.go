package domain

import (
	"bytes"
	"encoding/json"
	"io"
)

type legacyNativeObjectRestored struct {
	Object  *legacyNativeObject
	Scanner legacyJSONValueScanner
	Source  LegacyByteSource
}

type legacyNativeObjectCheckpointField struct {
	Index         int                                      `json:"i"`
	Initialized   bool                                     `json:"initialized"`
	Present       bool                                     `json:"present"`
	Value         legacyNativeCheckpointScalar             `json:"value"`
	Integer       int64                                    `json:"integer"`
	Boolean       bool                                     `json:"boolean"`
	BooleanAlias  uint8                                    `json:"boolean_alias"`
	Dynamic       uint8                                    `json:"dynamic"`
	Block         *legacyNativeObjectCheckpointBlockField  `json:"block"`
	Nested        *legacyNativeObjectCheckpointNestedField `json:"nested,omitempty"`
	Any           *legacyNativeObjectCheckpointAnyField    `json:"any,omitempty"`
	Event         *legacyNativeObjectCheckpointEventField  `json:"event,omitempty"`
	InitialGraph  uint8                                    `json:"initial_graph,omitempty"`
	Strings       legacyNativeCheckpointStrings            `json:"strings"`
	StringBinding uint8                                    `json:"string_binding"`
	StringParser  legacyNativeCheckpointRef                `json:"string_parser"`
}

type legacyNativeObjectCheckpointFields []legacyNativeObjectCheckpointField

func (fields *legacyNativeObjectCheckpointFields) UnmarshalJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return ErrIntegrity
	}
	values := make(legacyNativeObjectCheckpointFields, 0, 32)
	for decoder.More() {
		if len(values) == 32 {
			return ErrIntegrity
		}
		var field legacyNativeObjectCheckpointField
		if err := decoder.Decode(&field); err != nil {
			return ErrIntegrity
		}
		values = append(values, field)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return ErrIntegrity
	}
	if _, err = decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	*fields = values
	return nil
}

type legacyNativeObjectCheckpointControl struct {
	Version             uint8                              `json:"v"`
	Binding             ContentHash                        `json:"b"`
	SourceBytes         int                                `json:"n"`
	Model               uint8                              `json:"model"`
	Schema              ContentHash                        `json:"schema"`
	ParentDepth         int                                `json:"parents"`
	TrackPresence       bool                               `json:"track_presence"`
	Presence            eventFieldPresence                 `json:"presence"`
	PagedEvents         bool                               `json:"paged"`
	DeferTypeErrors     bool                               `json:"defer_errors"`
	SkipValue           bool                               `json:"skip"`
	FieldLimit          int                                `json:"limit"`
	Prefix              bool                               `json:"prefix"`
	Position            int                                `json:"position"`
	End                 int                                `json:"end"`
	ValueStart          int                                `json:"from"`
	ValueEnd            int                                `json:"to"`
	Field               int                                `json:"field"`
	Phase               uint8                              `json:"phase"`
	ValueFirst          byte                               `json:"first"`
	KeyStarted          bool                               `json:"key_started"`
	KeyOverflow         bool                               `json:"key_overflow"`
	ValueStarted        bool                               `json:"value_started"`
	Key                 []byte                             `json:"key"`
	RawCapacity         int                                `json:"raw_capacity"`
	Fields              legacyNativeObjectCheckpointFields `json:"fields"`
	Scanner             legacyNativeCheckpointRef          `json:"scanner"`
	Scalar              legacyNativeCheckpointScalar       `json:"scalar"`
	HasScalar           bool                               `json:"has_scalar"`
	ListActive          bool                               `json:"list_active"`
	HasStringCache      bool                               `json:"has_string_cache"`
	BlockActive         bool                               `json:"block_active"`
	HasBlockCache       bool                               `json:"has_block_cache"`
	EventActive         bool                               `json:"event_active,omitempty"`
	HasEventCache       bool                               `json:"has_event_cache,omitempty"`
	NestedActive        bool                               `json:"nested_active,omitempty"`
	HasNestedCache      bool                               `json:"has_nested_cache,omitempty"`
	ContainerActive     bool                               `json:"container_active,omitempty"`
	HasContainerCache   bool                               `json:"has_container_cache,omitempty"`
	AnyGraphNamespace   uint8                              `json:"any_graph_namespace,omitempty"`
	HasFloatNumber      bool                               `json:"has_float_number,omitempty"`
	FloatNumber         *legacyNativeCheckpointRef         `json:"float_number,omitempty"`
	InitialGraphCount   uint8                              `json:"initial_graph_count,omitempty"`
	InitialGraphs       *legacyNativeCheckpointRef         `json:"initial_graphs,omitempty"`
	DestinationPresence eventFieldPresence                 `json:"destination_presence"`
	Number              legacyNativeCheckpointRef          `json:"number"`
	Error               legacyNativeCheckpointRef          `json:"error"`
}
