package remoteconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type WriteMode int

const (
	WriteLegacy WriteMode = iota
	WriteBoth
	WriteZstd
)

type ReadMode int

const (
	ReadLegacy ReadMode = iota
	ReadZstdFallbackLegacy
)

type Source string

const (
	SourceLegacy         Source = "legacy"
	SourceZstd           Source = "zstd"
	SourceLegacyFallback Source = "legacy_fallback"
)

var ErrNotFound = errors.New("remote config not found")

type Store struct {
	rdb   redis.Cmdable
	ttl   time.Duration
	write WriteMode
	read  ReadMode
}

func NewStore(rdb redis.Cmdable, ttl time.Duration, write WriteMode, read ReadMode) *Store {
	return &Store{rdb: rdb, ttl: ttl, write: write, read: read}
}

func HashKey(playerID string) string { return "remote-config-hash:" + playerID }
func LegacyKey(hash string) string   { return "remote-config:" + hash }
func ZstdKey(hash string) string     { return "remote-config:z:" + hash }

func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func (s *Store) Store(ctx context.Context, playerID string, cfg any) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	hash := Hash(raw)

	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		if s.write != WriteZstd {
			p.Set(ctx, LegacyKey(hash), raw, s.ttl)
		}
		if s.write != WriteLegacy {
			p.Set(ctx, ZstdKey(hash), Compress(raw), s.ttl)
		}
		p.Set(ctx, HashKey(playerID), hash, s.ttl)
		return nil
	})
	return hash, err
}

func (s *Store) Retrieve(ctx context.Context, playerID string, out any) (Source, error) {
	hash, err := s.rdb.Get(ctx, HashKey(playerID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	source := SourceLegacy
	if s.read == ReadZstdFallbackLegacy {
		blob, err := s.rdb.Get(ctx, ZstdKey(hash)).Bytes()
		switch {
		case err == nil:
			if raw, decErr := Decompress(blob); decErr == nil {
				return SourceZstd, json.Unmarshal(raw, out)
			}
		case !errors.Is(err, redis.Nil):
			return "", err
		}
		source = SourceLegacyFallback
	}

	raw, err := s.rdb.Get(ctx, LegacyKey(hash)).Bytes()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return source, json.Unmarshal(raw, out)
}
