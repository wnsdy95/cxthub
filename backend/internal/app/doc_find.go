package app

const docFindScanBytes = 256 << 10
const docFindFragmentBytes = 64 << 10
const docFindScanEvents = 128

type docFindProgress struct {
	From, To, Index, Field, Byte int
	Representation               string
}
