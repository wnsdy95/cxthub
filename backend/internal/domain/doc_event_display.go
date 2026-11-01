package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const MaxDocEventDisplayPreparationBytes = 2048
const MaxDocEventDisplayStepObjects = 32
const MaxDocEventDisplayStepBytes = 1 << 20

type DocEventDisplaySource struct {
	Version        int         `json:"version"`
	Profile        string      `json:"profile"`
	EventHash      ContentHash `json:"event_hash"`
	EventBytes     int         `json:"event_bytes"`
	Kind           EventKind   `json:"kind"`
	EmptyReasoning bool        `json:"empty_reasoning"`
	Summary        bool        `json:"summary,omitempty"`
}

func (source DocEventDisplaySource) Validate() error {
	if source.Version != 1 || source.Profile != DocDisplayProjectionProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.Kind != EventReasoning && source.Kind != EventMessage && source.Kind != EventToolCall && source.Kind != EventToolResult || source.Kind != EventMessage && source.Summary || source.Kind != EventReasoning && source.EmptyReasoning {
		return ErrIntegrity
	}
	return nil
}

func (source DocEventDisplaySource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocEventDisplaySource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type DocEventDisplayPreparation struct {
	Version    int                    `json:"version"`
	Source     DocEventDisplaySource  `json:"source"`
	Phase      string                 `json:"phase"`
	Active     ContentHash            `json:"active,omitempty"`
	Input      *DocDecodedTextSource  `json:"input,omitempty"`
	Decoded    ContentHash            `json:"decoded,omitempty"`
	Plan       ContentHash            `json:"plan,omitempty"`
	Generation ContentHash            `json:"generation,omitempty"`
	Message    *DocMessageInputSource `json:"message,omitempty"`
	Inputs     ContentHash            `json:"inputs,omitempty"`
	Child      ContentHash            `json:"child,omitempty"`
	ToolFields ContentHash            `json:"tool_fields,omitempty"`
	ToolTree   *DocToolTreeSource     `json:"tool_tree,omitempty"`
	ToolKeys   *DocToolKeyMapSource   `json:"tool_keys,omitempty"`
	ToolOrders *DocToolOrdersSource   `json:"tool_orders,omitempty"`
	ToolValue  ContentHash            `json:"tool_value,omitempty"`
	ToolPlan   ContentHash            `json:"tool_plan,omitempty"`
	ToolEvent  ContentHash            `json:"tool_event,omitempty"`
}

func NewDocEventDisplayPreparation(entry DocRangeEntry) (DocEventDisplayPreparation, error) {
	if entry.Kind != EventReasoning && entry.Kind != EventMessage && entry.Kind != EventToolCall && entry.Kind != EventToolResult {
		return DocEventDisplayPreparation{}, ErrDocFindDisplayUnavailable
	}
	result := DocEventDisplayPreparation{Version: 1, Phase: "discovery", Source: DocEventDisplaySource{
		Version: 1, Profile: DocDisplayProjectionProfile, EventHash: entry.Hash, EventBytes: entry.Length, Kind: entry.Kind, EmptyReasoning: entry.Kind == EventReasoning && entry.EmptyReasoning, Summary: entry.Kind == EventMessage && (entry.CompactSummary || entry.SeedSummary),
	}}
	if entry.Kind == EventToolCall || entry.Kind == EventToolResult {
		result.Phase = "tool_discovery"
	}
	return result, result.Validate()
}

func (state DocEventDisplayPreparation) Validate() error {
	if state.Version != 1 || state.Source.Validate() != nil {
		return ErrIntegrity
	}
	for _, hash := range []ContentHash{state.Active, state.Decoded, state.Plan, state.Generation, state.Inputs, state.Child, state.ToolFields, state.ToolValue, state.ToolPlan, state.ToolEvent} {
		if hash != "" && ValidateContentHash(hash) != nil {
			return ErrIntegrity
		}
	}
	if state.Input != nil && (state.Input.Validate() != nil || state.Input.EventHash != state.Source.EventHash || state.Input.EventBytes != state.Source.EventBytes) {
		return ErrIntegrity
	}
	if state.Source.Kind == EventToolCall || state.Source.Kind == EventToolResult {
		return state.validateTool()
	}
	if state.ToolTree != nil || state.ToolKeys != nil || state.ToolOrders != nil || state.ToolFields != "" || state.ToolValue != "" || state.ToolPlan != "" || state.ToolEvent != "" {
		return ErrIntegrity
	}
	if state.Source.Kind == EventMessage {
		return state.validateMessage()
	}
	if state.Message != nil || state.Inputs != "" || state.Child != "" {
		return ErrIntegrity
	}
	valid := false
	switch state.Phase {
	case "discovery":
		valid = state.Input == nil && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "decoding":
		valid = !state.Source.EmptyReasoning && state.Active != "" && state.Input != nil && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "markdown":
		valid = !state.Source.EmptyReasoning && state.Active != "" && state.Input == nil && state.Decoded != "" && state.Plan == "" && state.Generation == ""
	case "assembly":
		valid = !state.Source.EmptyReasoning && state.Active != "" && state.Input == nil && state.Decoded != "" && state.Plan != "" && state.Generation == ""
	case "completed":
		valid = state.Active == "" && state.Input == nil && state.Plan != "" && state.Generation != "" && (state.Source.EmptyReasoning == (state.Decoded == ""))
	}
	if !valid {
		return ErrIntegrity
	}
	return nil
}

func (state DocEventDisplayPreparation) Encode() ([]byte, error) {
	if state.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocEventDisplayPreparationBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocEventDisplayPreparation(expected ContentHash, raw []byte) (DocEventDisplayPreparation, error) {
	var state DocEventDisplayPreparation
	if len(raw) == 0 || len(raw) > MaxDocEventDisplayPreparationBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocEventDisplayPreparation{}, ErrIntegrity
	}
	canonical, err := state.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocEventDisplayPreparation{}, ErrIntegrity
	}
	return state, nil
}

type DocEventDisplayStep struct {
	prior   ContentHash
	state   DocEventDisplayPreparation
	objects []DocRangeObject
	tool    *DocToolTreeScanStep
	keys    *DocToolKeyMapStep
	orders  *DocToolOrdersStep
	catalog *DocDisplayCatalogUse
}

func (step DocEventDisplayStep) Prior() ContentHash { return step.prior }

func (step DocEventDisplayStep) WithCatalogUse(use DocDisplayCatalogUse) (DocEventDisplayStep, error) {
	source, err := step.state.Source.Hash()
	if err != nil || use.Validate() != nil || ValidateContentHash(step.prior) != nil || use.Source != source {
		return DocEventDisplayStep{}, ErrIntegrity
	}
	if step.catalog != nil && *step.catalog != use {
		return DocEventDisplayStep{}, ErrConflict
	}
	step.catalog = &use
	return step, nil
}

func (step DocEventDisplayStep) CatalogUse() (DocDisplayCatalogUse, bool) {
	if step.catalog == nil {
		return DocDisplayCatalogUse{}, false
	}
	return *step.catalog, true
}

func (step DocEventDisplayStep) Preparation() DocEventDisplayPreparation {
	result := step.state
	if result.Input != nil {
		input := *result.Input
		result.Input = &input
	}
	if result.Message != nil {
		message := *result.Message
		result.Message = &message
	}
	if result.ToolTree != nil {
		tree := *result.ToolTree
		result.ToolTree = &tree
	}
	if result.ToolKeys != nil {
		keys := *result.ToolKeys
		result.ToolKeys = &keys
	}
	if result.ToolOrders != nil {
		orders := *result.ToolOrders
		result.ToolOrders = &orders
	}
	return result
}

func (step DocEventDisplayStep) ToolOrders() (DocToolOrdersSource, DocToolOrdersStep, bool) {
	if step.orders == nil || step.state.ToolOrders == nil {
		return DocToolOrdersSource{}, DocToolOrdersStep{}, false
	}
	return *step.state.ToolOrders, *step.orders, true
}

func (step DocEventDisplayStep) ToolKeyMap() (DocToolKeyMapSource, DocToolKeyMapStep, bool) {
	if step.keys == nil || step.state.ToolKeys == nil {
		return DocToolKeyMapSource{}, DocToolKeyMapStep{}, false
	}
	return *step.state.ToolKeys, *step.keys, true
}

func (step DocEventDisplayStep) ToolTreeScan() (DocToolTreeSource, DocToolTreeScanStep, bool) {
	if step.tool == nil || step.state.ToolTree == nil {
		return DocToolTreeSource{}, DocToolTreeScanStep{}, false
	}
	return *step.state.ToolTree, *step.tool, true
}

func (step DocEventDisplayStep) Objects() []DocRangeObject {
	result := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		result[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return result
}

func ValidateDocEventDisplayPublication(ctx context.Context, state DocEventDisplayPreparation, load func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayProjectionGeneration, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocDisplayProjectionGeneration{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayProjectionGeneration{}, err
	}
	if state.Validate() != nil || state.Phase != "completed" || load == nil {
		return DocDisplayProjectionGeneration{}, ErrIntegrity
	}
	reader := docEventDisplayBuilder{ctx: ctx, state: state, load: load}
	raw, err := reader.read(state.Generation, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDisplayProjectionGeneration{}, err
	}
	generation, err := DecodeDocDisplayProjectionGeneration(state.Generation, raw)
	if err != nil || generation.Source.EventHash != state.Source.EventHash || generation.Source.EventBytes != state.Source.EventBytes || generation.Source.Plan != state.Plan {
		return DocDisplayProjectionGeneration{}, ErrIntegrity
	}
	if state.Source.EmptyReasoning {
		raw, err := reader.read(state.Plan, MaxDocFieldDiscoveryStateBytes)
		if err != nil {
			return DocDisplayProjectionGeneration{}, err
		}
		discovery, err := DecodeDocFieldDiscoveryState(state.Plan, raw)
		if err != nil || !discovery.Done || discovery.EventHash != state.Source.EventHash || discovery.EventBytes != state.Source.EventBytes || discovery.Kind != EventReasoning || generation.Source.TextBytes != 0 || generation.Pages.Count != 0 {
			return DocDisplayProjectionGeneration{}, ErrIntegrity
		}
		if _, err := discovery.FindFields(); err != nil {
			return DocDisplayProjectionGeneration{}, err
		}
	} else {
		input, err := reader.assemblyInput()
		if err != nil {
			return DocDisplayProjectionGeneration{}, err
		}
		if generation.Source != input.Source() {
			return DocDisplayProjectionGeneration{}, ErrIntegrity
		}
	}
	return generation, ctx.Err()
}
