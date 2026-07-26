package storage

import "github.com/klauspost/compress/zstd"

// doc body at-rest zstd compression — JSON scripts reduce by 5~10× (empirically 849MB/week).
// content-hash is canonical JSON (uncompressed) based, invariant across storage representations,
// and reads transparently handle new compressed and legacy uncompressed files via zstd magic (0x28B52FFD).
// EncodeAll/DecodeAll are safe for concurrent calls (shared instance OK).
var (
	zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zstdDec, _ = zstd.NewReader(nil)
)

func docCompress(b []byte) []byte { return zstdEnc.EncodeAll(b, make([]byte, 0, len(b)/4)) }

func docDecompress(b []byte) ([]byte, error) {
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xB5 && b[2] == 0x2F && b[3] == 0xFD {
		return zstdDec.DecodeAll(b, nil)
	}
	return b, nil // legacy uncompressed (JSON original)
}
