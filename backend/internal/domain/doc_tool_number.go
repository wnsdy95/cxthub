package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
)

const DocToolNumberProfile = "public-tool-number-v1"
const DocToolNumberStepSourceBytes = 64 << 10
const MaxDocToolNumberStateBytes = 64 << 10
const DocToolNumberRetainedDigits = 2048
const docToolNumberCoefficientLimbs = 128
const docToolNumberHashStateBytes = 4 + sha256.Size + sha256.BlockSize + 8

type DocToolNumberSource struct {
	Version int               `json:"version"`
	Profile string            `json:"profile"`
	Tree    DocToolTreeSource `json:"tree"`
	Node    DocToolTreeNode   `json:"node"`
}

func cloneDocToolNumberSource(source DocToolNumberSource) DocToolNumberSource {
	if source.Node.Key != nil {
		key := *source.Node.Key
		source.Node.Key = &key
	}
	return source
}

func NewDocToolNumberSource(tree DocToolTreeSource, node DocToolTreeNode) (DocToolNumberSource, error) {
	source := DocToolNumberSource{Version: 1, Profile: DocToolNumberProfile, Tree: tree, Node: node}
	if source.Validate() != nil {
		return DocToolNumberSource{}, ErrIntegrity
	}
	return cloneDocToolNumberSource(source), nil
}

func (source DocToolNumberSource) Validate() error {
	if source.Version != 1 || source.Profile != DocToolNumberProfile || source.Node.Type != DocToolValueNumber || source.Node.Validate(source.Tree) != nil {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolNumberSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolNumberSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolNumberSource(expected ContentHash, raw []byte) (DocToolNumberSource, error) {
	var source DocToolNumberSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolNumberSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolNumberSource{}, ErrIntegrity
	}
	return source, nil
}

type DocToolNumberState struct {
	Version           int                                   `json:"version"`
	Source            ContentHash                           `json:"source"`
	Next              int                                   `json:"next"`
	Phase             string                                `json:"phase"`
	Negative          bool                                  `json:"negative"`
	IntegerZero       bool                                  `json:"integer_zero"`
	IntegerDigits     int                                   `json:"integer_digits"`
	Dot               bool                                  `json:"dot"`
	FractionDigits    int                                   `json:"fraction_digits"`
	LeadingZeros      int                                   `json:"leading_zeros"`
	SignificantDigits int                                   `json:"significant_digits"`
	RetainedDigits    int                                   `json:"retained_digits"`
	Coefficient       [docToolNumberCoefficientLimbs]uint64 `json:"coefficient"`
	Sticky            bool                                  `json:"sticky"`
	ExponentMark      bool                                  `json:"exponent_mark"`
	ExponentSign      byte                                  `json:"exponent_sign"`
	ExponentDigits    int                                   `json:"exponent_digits"`
	Exponent          int64                                 `json:"exponent"`
	HashState         [docToolNumberHashStateBytes]byte     `json:"hash_state"`
	Digest            ContentHash                           `json:"digest"`
	Complete          bool                                  `json:"complete"`
}

func NewDocToolNumberState(source DocToolNumberSource) (DocToolNumberState, error) {
	identity, err := source.Hash()
	if err != nil {
		return DocToolNumberState{}, err
	}
	state := DocToolNumberState{Version: 1, Source: identity, Next: source.Node.Value.From, Phase: "start"}
	raw, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil || len(raw) != len(state.HashState) {
		return DocToolNumberState{}, ErrIntegrity
	}
	copy(state.HashState[:], raw)
	return state, nil
}

func (state DocToolNumberState) Done() bool { return state.Complete }

func docToolNumberBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (state DocToolNumberState) Validate(source DocToolNumberSource) error {
	identity, err := source.Hash()
	span := source.Node.Value
	if err != nil || state.Version != 1 || state.Source != identity || state.Next < span.From || state.Next > span.To || state.Complete != (state.Next == span.To) {
		return ErrIntegrity
	}
	consumed := state.Next - span.From
	for _, count := range []int{state.IntegerDigits, state.FractionDigits, state.LeadingZeros, state.SignificantDigits, state.RetainedDigits, state.ExponentDigits} {
		if count < 0 || count > consumed {
			return ErrIntegrity
		}
	}
	if int64(state.IntegerDigits)+int64(state.FractionDigits)+int64(state.ExponentDigits)+int64(docToolNumberBool(state.Negative)+docToolNumberBool(state.Dot)+docToolNumberBool(state.ExponentMark)+docToolNumberBool(state.ExponentSign != 0)) != int64(consumed) || int64(state.LeadingZeros)+int64(state.SignificantDigits) != int64(state.IntegerDigits)+int64(state.FractionDigits) || state.RetainedDigits != min(state.SignificantDigits, DocToolNumberRetainedDigits) || state.Sticky && state.SignificantDigits <= DocToolNumberRetainedDigits {
		return ErrIntegrity
	}
	coefficient := docToolNumberCoefficientText(state.Coefficient)
	if state.RetainedDigits == 0 {
		if coefficient != "0" || state.Sticky {
			return ErrIntegrity
		}
	} else if coefficient == "0" || len(coefficient) != state.RetainedDigits {
		return ErrIntegrity
	}
	if state.IntegerZero && state.IntegerDigits != 1 || state.IntegerDigits > 0 && (state.LeadingZeros > 0) != state.IntegerZero || !state.Dot && state.FractionDigits != 0 || !state.ExponentMark && (state.ExponentSign != 0 || state.ExponentDigits != 0) || state.ExponentSign != 0 && state.ExponentSign != '+' && state.ExponentSign != '-' || state.ExponentMark && (state.IntegerDigits == 0 || state.Dot && state.FractionDigits == 0) {
		return ErrIntegrity
	}
	maximumExponent := int64(0)
	for index := 0; index < min(state.ExponentDigits, 10); index++ {
		maximumExponent = min(docToolNumberExponentLimit(source), maximumExponent*10+9)
	}
	if state.Exponent < 0 || state.Exponent > maximumExponent {
		return ErrIntegrity
	}
	switch state.Phase {
	case "start":
		if consumed != 0 {
			return ErrIntegrity
		}
	case "sign":
		if consumed != 1 || !state.Negative || state.IntegerDigits != 0 {
			return ErrIntegrity
		}
	case "zero", "integer":
		if state.IntegerDigits == 0 || state.Dot || state.ExponentMark || (state.Phase == "zero") != state.IntegerZero {
			return ErrIntegrity
		}
	case "dot", "fraction":
		if state.IntegerDigits == 0 || !state.Dot || state.ExponentMark || (state.Phase == "dot") != (state.FractionDigits == 0) {
			return ErrIntegrity
		}
	case "exponent", "exponent_sign", "exponent_digits":
		if !state.ExponentMark || (state.Phase == "exponent_digits") != (state.ExponentDigits > 0) || state.Phase == "exponent" && state.ExponentSign != 0 || state.Phase == "exponent_sign" && state.ExponentSign == 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	if consumed == 0 {
		initial, err := NewDocToolNumberState(source)
		if err != nil || state != initial {
			return ErrIntegrity
		}
	} else if state.Complete {
		if !state.accepting() || ValidateContentHash(state.Digest) != nil || state.HashState != [docToolNumberHashStateBytes]byte{} {
			return ErrIntegrity
		}
	} else if consumed%DocToolNumberStepSourceBytes != 0 || state.Digest != "" || !validDocFieldHashState(state.HashState[:], consumed) {
		return ErrIntegrity
	}
	return nil
}

func (state DocToolNumberState) Encode(source DocToolNumberSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocToolNumberStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolNumberState(source DocToolNumberSource, expected ContentHash, raw []byte) (DocToolNumberState, error) {
	var state DocToolNumberState
	if len(raw) == 0 || len(raw) > MaxDocToolNumberStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolNumberState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolNumberState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolNumberStep struct {
	source    DocToolNumberSource
	prior     ContentHash
	state     DocToolNumberState
	readBytes int
}

func (step DocToolNumberStep) Source() DocToolNumberSource {
	return cloneDocToolNumberSource(step.source)
}
func (step DocToolNumberStep) Prior() ContentHash        { return step.prior }
func (step DocToolNumberStep) State() DocToolNumberState { return step.state }
func (step DocToolNumberStep) ReadBytes() int            { return step.readBytes }

type DocToolNumberPlan struct {
	Version int                 `json:"version"`
	Source  DocToolNumberSource `json:"source"`
	Digest  ContentHash         `json:"digest"`
	Text    string              `json:"text"`
	Linear  bool                `json:"linear"`
}

func (plan DocToolNumberPlan) Validate() error {
	if plan.Version != 1 || plan.Source.Validate() != nil || ValidateContentHash(plan.Digest) != nil || len(plan.Text) == 0 || len(plan.Text) > 32 || !docToolNumberCanonicalText(plan.Text) || plan.Linear != (len(plan.Text) == plan.Source.Node.Value.To-plan.Source.Node.Value.From && HashContent([]byte(plan.Text)) == plan.Digest) {
		return ErrIntegrity
	}
	return nil
}

func (plan DocToolNumberPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(plan)
}

func DecodeDocToolNumberPlan(expected ContentHash, raw []byte) (DocToolNumberPlan, error) {
	var plan DocToolNumberPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocToolNumberPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolNumberPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan DocToolNumberPlan) SourceRange(offset, length int) (DocFindSpan, error) {
	if plan.Validate() != nil || offset < 0 || length <= 0 || offset > len(plan.Text)-length {
		return DocFindSpan{}, ErrIntegrity
	}
	span := plan.Source.Node.Value
	if plan.Linear {
		span.From += offset
		span.To = span.From + length
	}
	return span, nil
}

func FinishDocToolNumber(ctx context.Context, source DocToolNumberSource, state DocToolNumberState) (result DocToolNumberPlan, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolNumberPlan{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if state.Validate(source) != nil || !state.Done() {
		return result, ErrIntegrity
	}
	text, err := docToolNumberText(state)
	if err != nil {
		return result, err
	}
	result = DocToolNumberPlan{Version: 1, Source: cloneDocToolNumberSource(source), Digest: state.Digest, Text: text, Linear: len(text) == source.Node.Value.To-source.Node.Value.From && HashContent([]byte(text)) == state.Digest}
	if result.Validate() != nil {
		return DocToolNumberPlan{}, ErrIntegrity
	}
	return result, nil
}

func docToolNumberHashText(sum []byte) ContentHash {
	return ContentHash("sha256:" + hex.EncodeToString(sum))
}
