package domain

import (
	"context"
	"unsafe"
)

const legacyNativeFloat64DecimalProvenance = "Go 1.26.10 src/internal/strconv/decimal.go: trim, rightShift, leftcheats, prefixIsLessThan, leftShift, Shift, shouldRoundUp, RoundedInteger; src/internal/strconv/atof.go: powtab, decimal.floatBits; 64-bit source operation order"

type legacyNativeFloat64DecimalPhase uint8

const (
	legacyNativeFloat64DecimalStart legacyNativeFloat64DecimalPhase = iota
	legacyNativeFloat64DecimalScaleDown
	legacyNativeFloat64DecimalScaleUp
	legacyNativeFloat64DecimalSubnormal
	legacyNativeFloat64DecimalRange
	legacyNativeFloat64DecimalShift
	legacyNativeFloat64DecimalRound
	legacyNativeFloat64DecimalDone
)

type legacyNativeFloat64Decimal struct {
	digits     [800]byte
	count      int
	point      int
	exponent   int
	shift      int
	shiftDelta int
	bits       uint64
	phase      legacyNativeFloat64DecimalPhase
	afterShift legacyNativeFloat64DecimalPhase
	negative   bool
	truncated  bool
	rejected   bool
}

func newLegacyNativeFloat64Decimal(digits [800]byte, count, point int, negative, truncated bool) (*legacyNativeFloat64Decimal, error) {
	if count < 0 || count > len(digits) || count > 0 && digits[0] == '0' {
		return nil, ErrIntegrity
	}
	for index := 0; index < count; index++ {
		if digits[index] < '0' || digits[index] > '9' {
			return nil, ErrIntegrity
		}
	}
	return &legacyNativeFloat64Decimal{digits: digits, count: count, point: point, negative: negative, truncated: truncated}, nil
}

func (decimal *legacyNativeFloat64Decimal) step(ctx context.Context, input *legacyNormalizationInput) (bits uint64, complete, rejected bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, false, err
	}
	if decimal == nil || input == nil || decimal.phase > legacyNativeFloat64DecimalDone || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes {
		return 0, false, false, ErrIntegrity
	}
	if input.failure != nil {
		return 0, false, false, input.failure
	}
	if decimal.phase == legacyNativeFloat64DecimalDone {
		if err := ctx.Err(); err != nil {
			return 0, false, false, err
		}
		return decimal.bits, true, decimal.rejected, nil
	}
	if decimal.transitionWork() > ConversationManifestChunkBytes-input.workBytes {
		return 0, false, false, ctx.Err()
	}
	shadow := *decimal
	input.workBytes += 2*int(unsafe.Sizeof(shadow)) + 1 + shadow.advance()
	if err := ctx.Err(); err != nil {
		return 0, false, false, err
	}
	input.workBytes += 2 * int(unsafe.Sizeof(shadow))
	*decimal = shadow
	if decimal.phase != legacyNativeFloat64DecimalDone {
		return 0, false, false, nil
	}
	return decimal.bits, true, decimal.rejected, nil
}

func (decimal *legacyNativeFloat64Decimal) transitionWork() int {
	work := 4*int(unsafe.Sizeof(*decimal)) + 1
	switch decimal.phase {
	case legacyNativeFloat64DecimalScaleDown:
		if decimal.point > 0 {
			return work + 4*decimal.count + 8*27 + 128
		}
	case legacyNativeFloat64DecimalScaleUp:
		if decimal.point < 0 || decimal.point == 0 && decimal.digits[0] < '5' {
			return work + 4*decimal.count + 8*27 + 128
		}
	case legacyNativeFloat64DecimalShift:
		return work + 4*decimal.count + 8*60 + 128
	case legacyNativeFloat64DecimalRound:
		return work + 24
	}
	return work
}

var legacyNativeFloat64DecimalPowers = [...]int{1, 3, 6, 9, 13, 16, 19, 23, 26}

func (decimal *legacyNativeFloat64Decimal) advance() int {
	switch decimal.phase {
	case legacyNativeFloat64DecimalStart:
		switch {
		case decimal.count == 0:
			decimal.finish(0, -1023, false)
		case decimal.point > 310:
			decimal.finish(0, 1024, true)
		case decimal.point < -330:
			decimal.finish(0, -1023, false)
		default:
			decimal.phase = legacyNativeFloat64DecimalScaleDown
		}
	case legacyNativeFloat64DecimalScaleDown:
		if decimal.point > 0 {
			shift := 27
			if decimal.point < len(legacyNativeFloat64DecimalPowers) {
				shift = legacyNativeFloat64DecimalPowers[decimal.point]
			}
			work := decimal.shiftRight(uint(shift))
			decimal.exponent += shift
			return work
		}
		decimal.phase = legacyNativeFloat64DecimalScaleUp
	case legacyNativeFloat64DecimalScaleUp:
		if decimal.point < 0 || decimal.point == 0 && decimal.digits[0] < '5' {
			shift := 27
			if -decimal.point < len(legacyNativeFloat64DecimalPowers) {
				shift = legacyNativeFloat64DecimalPowers[-decimal.point]
			}
			work := decimal.shiftLeft(uint(shift))
			decimal.exponent -= shift
			return work
		}
		decimal.exponent--
		decimal.phase = legacyNativeFloat64DecimalSubnormal
	case legacyNativeFloat64DecimalSubnormal:
		if decimal.exponent < -1022 {
			decimal.shift = decimal.exponent + 1022
			decimal.shiftDelta = -decimal.shift
			decimal.afterShift = legacyNativeFloat64DecimalRange
			decimal.phase = legacyNativeFloat64DecimalShift
		} else {
			decimal.phase = legacyNativeFloat64DecimalRange
		}
	case legacyNativeFloat64DecimalRange:
		if decimal.exponent+1023 >= 2047 {
			decimal.finish(0, 1024, true)
		} else {
			decimal.shift = 53
			decimal.afterShift = legacyNativeFloat64DecimalRound
			decimal.phase = legacyNativeFloat64DecimalShift
		}
	case legacyNativeFloat64DecimalShift:
		work := 0
		switch {
		case decimal.count == 0:
			decimal.shift = 0
		case decimal.shift > 0:
			shift := min(decimal.shift, 60)
			work = decimal.shiftLeft(uint(shift))
			decimal.shift -= shift
		case decimal.shift < 0:
			shift := min(-decimal.shift, 60)
			work = decimal.shiftRight(uint(shift))
			decimal.shift += shift
		}
		if decimal.shift == 0 {
			decimal.exponent += decimal.shiftDelta
			decimal.shiftDelta = 0
			decimal.phase = decimal.afterShift
		}
		return work
	case legacyNativeFloat64DecimalRound:
		mantissa, work := decimal.roundedInteger()
		if mantissa == 1<<53 {
			mantissa >>= 1
			decimal.exponent++
			if decimal.exponent+1023 >= 2047 {
				decimal.finish(0, 1024, true)
				return work
			}
		}
		if mantissa&(1<<52) == 0 {
			decimal.exponent = -1023
		}
		decimal.finish(mantissa, decimal.exponent, false)
		return work
	}
	return 0
}

func (decimal *legacyNativeFloat64Decimal) finish(mantissa uint64, exponent int, rejected bool) {
	decimal.phase, decimal.rejected = legacyNativeFloat64DecimalDone, rejected
	if rejected {
		decimal.bits = 0
		return
	}
	decimal.bits = mantissa & (uint64(1)<<52 - 1)
	decimal.bits |= uint64((exponent+1023)&2047) << 52
	if decimal.negative {
		decimal.bits |= uint64(1) << 63
	}
}

func (decimal *legacyNativeFloat64Decimal) trim() int {
	work := 0
	for decimal.count > 0 {
		work++
		if decimal.digits[decimal.count-1] != '0' {
			break
		}
		decimal.count--
	}
	if decimal.count == 0 {
		decimal.point = 0
	}
	return work
}

func (decimal *legacyNativeFloat64Decimal) shiftRight(shift uint) int {
	read, written, work := 0, 0, 0
	var accumulator uint64
	for ; accumulator>>shift == 0; read++ {
		if read >= decimal.count {
			if accumulator == 0 {
				decimal.count = 0
				return work
			}
			for accumulator>>shift == 0 {
				accumulator *= 10
				read++
				work++
			}
			break
		}
		accumulator = accumulator*10 + uint64(decimal.digits[read]) - '0'
		work++
	}
	decimal.point -= read - 1
	mask := uint64(1)<<shift - 1
	for ; read < decimal.count; read++ {
		character := uint64(decimal.digits[read])
		digit := accumulator >> shift
		accumulator &= mask
		decimal.digits[written] = byte(digit + '0')
		written++
		accumulator = accumulator*10 + character - '0'
		work += 2
	}
	for accumulator > 0 {
		digit := accumulator >> shift
		accumulator &= mask
		if written < len(decimal.digits) {
			decimal.digits[written] = byte(digit + '0')
			written++
		} else if digit > 0 {
			decimal.truncated = true
		}
		accumulator *= 10
		work++
	}
	decimal.count = written
	return work + decimal.trim()
}

type legacyNativeFloat64DecimalLeftCheat struct {
	delta  int
	cutoff string
}

var legacyNativeFloat64DecimalLeftCheats = [...]legacyNativeFloat64DecimalLeftCheat{
	{0, ""},
	{1, "5"},
	{1, "25"},
	{1, "125"},
	{2, "625"},
	{2, "3125"},
	{2, "15625"},
	{3, "78125"},
	{3, "390625"},
	{3, "1953125"},
	{4, "9765625"},
	{4, "48828125"},
	{4, "244140625"},
	{4, "1220703125"},
	{5, "6103515625"},
	{5, "30517578125"},
	{5, "152587890625"},
	{6, "762939453125"},
	{6, "3814697265625"},
	{6, "19073486328125"},
	{7, "95367431640625"},
	{7, "476837158203125"},
	{7, "2384185791015625"},
	{7, "11920928955078125"},
	{8, "59604644775390625"},
	{8, "298023223876953125"},
	{8, "1490116119384765625"},
	{9, "7450580596923828125"},
	{9, "37252902984619140625"},
	{9, "186264514923095703125"},
	{10, "931322574615478515625"},
	{10, "4656612873077392578125"},
	{10, "23283064365386962890625"},
	{10, "116415321826934814453125"},
	{11, "582076609134674072265625"},
	{11, "2910383045673370361328125"},
	{11, "14551915228366851806640625"},
	{12, "72759576141834259033203125"},
	{12, "363797880709171295166015625"},
	{12, "1818989403545856475830078125"},
	{13, "9094947017729282379150390625"},
	{13, "45474735088646411895751953125"},
	{13, "227373675443232059478759765625"},
	{13, "1136868377216160297393798828125"},
	{14, "5684341886080801486968994140625"},
	{14, "28421709430404007434844970703125"},
	{14, "142108547152020037174224853515625"},
	{15, "710542735760100185871124267578125"},
	{15, "3552713678800500929355621337890625"},
	{15, "17763568394002504646778106689453125"},
	{16, "88817841970012523233890533447265625"},
	{16, "444089209850062616169452667236328125"},
	{16, "2220446049250313080847263336181640625"},
	{16, "11102230246251565404236316680908203125"},
	{17, "55511151231257827021181583404541015625"},
	{17, "277555756156289135105907917022705078125"},
	{17, "1387778780781445675529539585113525390625"},
	{18, "6938893903907228377647697925567626953125"},
	{18, "34694469519536141888238489627838134765625"},
	{18, "173472347597680709441192448139190673828125"},
	{19, "867361737988403547205962240695953369140625"},
}

func legacyNativeFloat64DecimalPrefixLessThan(digits []byte, cutoff string) (bool, int) {
	work := 0
	for index := 0; index < len(cutoff); index++ {
		if index >= len(digits) {
			return true, work
		}
		digit, cutoffDigit := digits[index], cutoff[index]
		work += 2
		if digit != cutoffDigit {
			return digit < cutoffDigit, work
		}
	}
	return false, work
}

func (decimal *legacyNativeFloat64Decimal) shiftLeft(shift uint) int {
	cheat := legacyNativeFloat64DecimalLeftCheats[shift]
	delta := cheat.delta
	less, work := legacyNativeFloat64DecimalPrefixLessThan(decimal.digits[:decimal.count], cheat.cutoff)
	if less {
		delta--
	}
	read, written := decimal.count, decimal.count+delta
	var accumulator uint64
	for read--; read >= 0; read-- {
		accumulator += (uint64(decimal.digits[read]) - '0') << shift
		quotient := accumulator / 10
		remainder := accumulator - 10*quotient
		written--
		if written < len(decimal.digits) {
			decimal.digits[written] = byte(remainder + '0')
		} else if remainder != 0 {
			decimal.truncated = true
		}
		accumulator = quotient
		work += 2
	}
	for accumulator > 0 {
		quotient := accumulator / 10
		remainder := accumulator - 10*quotient
		written--
		if written < len(decimal.digits) {
			decimal.digits[written] = byte(remainder + '0')
		} else if remainder != 0 {
			decimal.truncated = true
		}
		accumulator = quotient
		work++
	}
	decimal.count += delta
	if decimal.count >= len(decimal.digits) {
		decimal.count = len(decimal.digits)
	}
	decimal.point += delta
	return work + decimal.trim()
}

func (decimal *legacyNativeFloat64Decimal) roundedInteger() (uint64, int) {
	if decimal.point > 20 {
		return 0xffffffffffffffff, 0
	}
	index, work := 0, 0
	var integer uint64
	for ; index < decimal.point && index < decimal.count; index++ {
		integer = integer*10 + uint64(decimal.digits[index]-'0')
		work++
	}
	for ; index < decimal.point; index++ {
		integer *= 10
		work++
	}
	if decimal.point < 0 || decimal.point >= decimal.count {
		return integer, work
	}
	work += 2
	if decimal.digits[decimal.point] == '5' && decimal.point+1 == decimal.count {
		if decimal.truncated {
			return integer + 1, work
		}
		if decimal.point > 0 {
			work++
			if (decimal.digits[decimal.point-1]-'0')%2 != 0 {
				integer++
			}
		}
	} else if decimal.digits[decimal.point] >= '5' {
		integer++
	}
	return integer, work
}

func (decimal *legacyNativeFloat64Decimal) retained(estimate *docRangeHeaderHeapEstimate) {
	if decimal != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*decimal)))
	}
}
