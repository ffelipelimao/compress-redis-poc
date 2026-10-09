package remoteconfig

import "github.com/klauspost/compress/zstd"

const maxDecodedSize = 16 << 20

// Encoder and Decoder hold no per-request state: EncodeAll/DecodeAll are safe for concurrent use.
var (
	encoder = mustEncoder()
	decoder = mustDecoder()
)

func mustEncoder() *zstd.Encoder {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		panic(err)
	}
	return e
}

func mustDecoder() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(maxDecodedSize))
	if err != nil {
		panic(err)
	}
	return d
}

func Compress(raw []byte) []byte {
	return encoder.EncodeAll(raw, make([]byte, 0, len(raw)/2))
}

func Decompress(blob []byte) ([]byte, error) {
	return decoder.DecodeAll(blob, nil)
}
