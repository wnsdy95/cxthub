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
