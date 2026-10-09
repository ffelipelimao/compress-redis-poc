package remoteconfig

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ffelipelimao/compress-redis-poc/internal/fakeconfig"
	"github.com/redis/go-redis/v9"
)

const (
	testPlayer = "player-1"
	testTTL    = time.Hour
)

func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; run with docker-compose (make test)")
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		rdb.FlushDB(context.Background())
		rdb.Close()
	})
	return rdb
}

func TestRolloutMatrix(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	cfg := fakeconfig.Generate(25_000, 42)

	tests := []struct {
		name    string
		write   WriteMode
		read    ReadMode
		mutate  func(t *testing.T, hash string)
		want    Source
		wantErr error
	}{
		{
			name:  "old pod writes, new pod reads: falls back to the legacy key",
			write: WriteLegacy,
			read:  ReadZstdFallbackLegacy,
			want:  SourceLegacyFallback,
		},
		{
			name:  "new pod writes both keys, old pod reads: legacy key still there",
			write: WriteBoth,
			read:  ReadLegacy,
			want:  SourceLegacy,
		},
		{
			name:  "new pod writes both keys, new pod reads: compressed key",
			write: WriteBoth,
			read:  ReadZstdFallbackLegacy,
			want:  SourceZstd,
		},
		{
			name:  "compressed key evicted: falls back to the legacy key",
			write: WriteBoth,
			read:  ReadZstdFallbackLegacy,
			mutate: func(t *testing.T, hash string) {
				if err := rdb.Del(ctx, ZstdKey(hash)).Err(); err != nil {
					t.Fatal(err)
				}
			},
			want: SourceLegacyFallback,
		},
		{
			name:  "compressed key corrupted: falls back to the legacy key",
			write: WriteBoth,
			read:  ReadZstdFallbackLegacy,
			mutate: func(t *testing.T, hash string) {
				if err := rdb.Set(ctx, ZstdKey(hash), "not-a-zstd-frame", testTTL).Err(); err != nil {
					t.Fatal(err)
				}
			},
			want: SourceLegacyFallback,
		},
		{
			name:  "zstd-only writer, new pod reads: compressed key",
			write: WriteZstd,
			read:  ReadZstdFallbackLegacy,
			want:  SourceZstd,
		},
		{
			name:    "zstd-only writer, old pod reads: miss, so dual write must last until every stack is new",
			write:   WriteZstd,
			read:    ReadLegacy,
			wantErr: ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := rdb.FlushDB(ctx).Err(); err != nil {
				t.Fatal(err)
			}

			hash, err := NewStore(rdb, testTTL, tt.write, ReadLegacy).Store(ctx, testPlayer, cfg)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			if tt.mutate != nil {
				tt.mutate(t, hash)
			}

			var got fakeconfig.Config
			source, err := NewStore(rdb, testTTL, WriteLegacy, tt.read).Retrieve(ctx, testPlayer, &got)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("retrieve error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if source != tt.want {
				t.Errorf("source = %q, want %q", source, tt.want)
			}
			if !reflect.DeepEqual(got, cfg) {
				t.Error("retrieved config differs from the stored one")
			}
		})
	}
}

func TestStoreWritesSmallerCompressedKeyWithTTL(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	cfg := fakeconfig.Generate(25_000, 42)

	hash, err := NewStore(rdb, testTTL, WriteBoth, ReadZstdFallbackLegacy).Store(ctx, testPlayer, cfg)
	if err != nil {
		t.Fatal(err)
	}

	legacyLen := rdb.StrLen(ctx, LegacyKey(hash)).Val()
	zstdLen := rdb.StrLen(ctx, ZstdKey(hash)).Val()
	if zstdLen == 0 || zstdLen >= legacyLen {
		t.Fatalf("STRLEN zstd = %d, legacy = %d: want 0 < zstd < legacy", zstdLen, legacyLen)
	}
	t.Logf("STRLEN legacy=%d zstd=%d (%.1f%% smaller)", legacyLen, zstdLen, 100*(1-float64(zstdLen)/float64(legacyLen)))

	for _, key := range []string{HashKey(testPlayer), LegacyKey(hash), ZstdKey(hash)} {
		if ttl := rdb.TTL(ctx, key).Val(); ttl <= 0 {
			t.Errorf("TTL(%s) = %v, want > 0", key, ttl)
		}
	}
}

func TestRetrieveUnknownPlayer(t *testing.T) {
	rdb := newRedis(t)

	var got fakeconfig.Config
	_, err := NewStore(rdb, testTTL, WriteBoth, ReadZstdFallbackLegacy).Retrieve(context.Background(), "nobody", &got)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
