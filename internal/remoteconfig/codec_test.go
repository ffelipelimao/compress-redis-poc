package remoteconfig

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ffelipelimao/compress-redis-poc/internal/fakeconfig"
	"github.com/klauspost/compress/zstd"
)

func fakeRaw(tb testing.TB) []byte {
	tb.Helper()
	raw, err := json.Marshal(fakeconfig.Generate(25_000, 42))
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func TestCompressRoundTrip(t *testing.T) {
	raw := fakeRaw(t)
	blob := Compress(raw)

	got, err := Decompress(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("round trip changed the payload")
	}
	t.Logf("raw=%d zstd=%d (%.1f%% smaller)", len(raw), len(blob), 100*(1-float64(len(blob))/float64(len(raw))))
}

func TestDecompressRejectsGarbage(t *testing.T) {
	if _, err := Decompress([]byte("not-a-zstd-frame")); err == nil {
		t.Fatal("want an error for a non-zstd payload")
	}
}

func BenchmarkUnmarshalLegacy(b *testing.B) {
	raw := fakeRaw(b)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		var cfg fakeconfig.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecompressAndUnmarshal(b *testing.B) {
	blob := Compress(fakeRaw(b))
	b.ReportAllocs()
	for b.Loop() {
		raw, err := Decompress(blob)
		if err != nil {
			b.Fatal(err)
		}
		var cfg fakeconfig.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecompressSharedDecoder(b *testing.B) {
	blob := Compress(fakeRaw(b))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := Decompress(blob); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDecompressNewDecoderPerRequest(b *testing.B) {
	blob := Compress(fakeRaw(b))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := d.DecodeAll(blob, nil); err != nil {
				b.Fatal(err)
			}
			d.Close()
		}
	})
}

func BenchmarkCompress(b *testing.B) {
	raw := fakeRaw(b)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		Compress(raw)
	}
}
