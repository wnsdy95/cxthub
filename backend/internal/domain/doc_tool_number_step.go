package domain

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"math/bits"
	"strconv"
)

func docToolNumberExponentLimit(source DocToolNumberSource) int64 {
	return int64(source.Node.Value.To-source.Node.Value.From) + 4096
}

func (state DocToolNumberState) accepting() bool {
	switch state.Phase {
	case "zero", "integer", "fraction", "exponent_digits":
		return true
	default:
		return false
	}
}

func (state *DocToolNumberState) coefficientDigit(character byte, fractional bool) error {
	if fractional {
		state.FractionDigits++
	} else {
		state.IntegerDigits++
	}
	digit := uint64(character - '0')
	if state.SignificantDigits == 0 && digit == 0 {
		state.LeadingZeros++
		return nil
	}
	state.SignificantDigits++
	if state.RetainedDigits == DocToolNumberRetainedDigits {
		state.Sticky = state.Sticky || digit != 0
		return nil
	}
	carry := digit
	for index, limb := range state.Coefficient {
		high, low := bits.Mul64(limb, 10)
		low, overflow := bits.Add64(low, carry, 0)
		state.Coefficient[index] = low
		carry = high + overflow
	}
	if carry != 0 {
		return ErrIntegrity
	}
	state.RetainedDigits++
	return nil
}

func (state *DocToolNumberState) consume(character byte, exponentLimit int64) error {
	digit := character >= '0' && character <= '9'
	switch state.Phase {
	case "start", "sign":
		if state.Phase == "start" && character == '-' {
			state.Negative, state.Phase = true, "sign"
			return nil
		}
		if !digit {
			return ErrIntegrity
		}
		state.IntegerZero = character == '0'
		state.Phase = "integer"
		if state.IntegerZero {
			state.Phase = "zero"
		}
		return state.coefficientDigit(character, false)
	case "zero", "integer":
		if digit && state.Phase == "integer" {
			return state.coefficientDigit(character, false)
		}
		if character == '.' {
			state.Dot, state.Phase = true, "dot"
			return nil
		}
	case "dot", "fraction":
		if digit {
			state.Phase = "fraction"
			return state.coefficientDigit(character, true)
		}
	case "exponent", "exponent_sign", "exponent_digits":
		if state.Phase == "exponent" && (character == '-' || character == '+') {
			state.ExponentSign, state.Phase = character, "exponent_sign"
			return nil
		}
		if !digit {
			return ErrIntegrity
		}
		state.ExponentDigits++
		value := int64(character - '0')
		if state.Exponent > (exponentLimit-value)/10 {
			state.Exponent = exponentLimit
		} else {
			state.Exponent = min(exponentLimit, state.Exponent*10+value)
		}
		state.Phase = "exponent_digits"
		return nil
	}
	if state.accepting() && (character == 'e' || character == 'E') {
		state.ExponentMark, state.Phase = true, "exponent"
		return nil
	}
	return ErrIntegrity
}

func StepDocToolNumber(ctx context.Context, source DocToolNumberSource, prior DocToolNumberState, loadRaw func(context.Context, int, int) ([]byte, error)) (result DocToolNumberStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolNumberStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	before, err := prior.Encode(source)
	if err != nil || prior.Done() || loadRaw == nil {
		return result, ErrIntegrity
	}
	length := min(DocToolNumberStepSourceBytes, source.Node.Value.To-prior.Next)
	raw, err := loadRaw(ctx, prior.Next, length)
	if err != nil {
		return result, err
	}
	if len(raw) != length {
		return result, ErrIntegrity
	}
	next := prior
	limit := docToolNumberExponentLimit(source)
	for index, character := range raw {
		if index%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
		if err := next.consume(character, limit); err != nil {
			return result, err
		}
	}
	next.Next += length
	next.Complete = next.Next == source.Node.Value.To
	if next.Complete && !next.accepting() {
		return result, ErrIntegrity
	}
	hasher := sha256.New()
	if hasher.(encoding.BinaryUnmarshaler).UnmarshalBinary(prior.HashState[:]) != nil {
		return result, ErrIntegrity
	}
	_, _ = hasher.Write(raw)
	if next.Complete {
		next.Digest = docToolNumberHashText(hasher.Sum(nil))
		next.HashState = [docToolNumberHashStateBytes]byte{}
	} else {
		encoded, err := hasher.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil || len(encoded) != len(next.HashState) {
			return result, ErrIntegrity
		}
		copy(next.HashState[:], encoded)
	}
	if next.Validate(source) != nil {
		return result, ErrIntegrity
	}
	return DocToolNumberStep{source: cloneDocToolNumberSource(source), prior: HashContent(before), state: next, readBytes: length}, nil
}

func docToolNumberCoefficientText(coefficient [docToolNumberCoefficientLimbs]uint64) string {
	var raw [docToolNumberCoefficientLimbs * 8]byte
	for index, limb := range coefficient {
		binary.BigEndian.PutUint64(raw[(len(coefficient)-index-1)*8:], limb)
	}
	return new(big.Int).SetBytes(raw[:]).String()
}

func docToolNumberText(state DocToolNumberState) (string, error) {
	if state.SignificantDigits == 0 {
		return "0", nil
	}
	coefficient := docToolNumberCoefficientText(state.Coefficient)
	exponent := state.Exponent
	if state.ExponentSign == '-' {
		exponent = -exponent
	}
	exponent += int64(state.SignificantDigits-state.RetainedDigits) - int64(state.FractionDigits)
	if state.Sticky {
		coefficient += "1"
		exponent--
	}
	exponent += int64(len(coefficient) - 1)
	if len(coefficient) > 1 {
		coefficient = coefficient[:1] + "." + coefficient[1:]
	}
	if state.Negative {
		coefficient = "-" + coefficient
	}
	input := coefficient + "e" + strconv.FormatInt(exponent, 10)
	if len(input) > DocToolNumberRetainedDigits+32 {
		return "", ErrIntegrity
	}
	number, err := strconv.ParseFloat(input, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) || math.IsNaN(number) {
		return "", ErrIntegrity
	}
	return docToolNumberFormat(number), nil
}

func docToolNumberFormat(number float64) string {
	if math.IsInf(number, 0) {
		return "null"
	}
	if number == 0 {
		return "0"
	}
	format := byte('f')
	if absolute := math.Abs(number); absolute < 1e-6 || absolute >= 1e21 {
		format = 'e'
	}
	var scratch [32]byte
	rendered := strconv.AppendFloat(scratch[:0], number, format, -1, 64)
	if count := len(rendered); count >= 4 && rendered[count-4] == 'e' && rendered[count-2] == '0' {
		rendered[count-2] = rendered[count-1]
		rendered = rendered[:count-1]
	}
	return string(rendered)
}

func docToolNumberCanonicalText(text string) bool {
	if text == "null" {
		return true
	}
	number, err := strconv.ParseFloat(text, 64)
	return err == nil && !math.IsInf(number, 0) && !math.IsNaN(number) && docToolNumberFormat(number) == text
}
