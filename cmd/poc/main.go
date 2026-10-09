package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/ffelipelimao/compress-redis-poc/internal/fakeconfig"
	"github.com/ffelipelimao/compress-redis-poc/internal/remoteconfig"
	"github.com/klauspost/compress/zstd"
	"github.com/redis/go-redis/v9"
)

const ttl = time.Hour

type payload struct {
	label  string
	raw    []byte
	newOut func() any
}

func main() {
	testing.Init()
	var (
		redisAddr   = flag.String("redis", envOr("REDIS_ADDR", "localhost:6379"), "redis address")
		redisDB     = flag.Int("db", 1, "redis DB used (and flushed) by the POC")
		configFile  = flag.String("config-file", "", "JSON file used as the config blob instead of the synthetic foo/bar one")
		size        = flag.Int("size", 25_000, "target size in bytes of the synthetic foo/bar config")
		players     = flag.Int("players", 1_000, "players seeded in redis")
		requests    = flag.Int("requests", 20_000, "operations per load run")
		concurrency = flag.Int("concurrency", 32, "concurrent clients")
		getsPerSec  = flag.Float64("blob-gets-per-sec", 1_000, "blob GET rate used to extrapolate monthly traffic")
		benchtime   = flag.String("benchtime", "300ms", "duration of each in-process codec benchmark (best of 3)")
	)
	flag.Parse()
	if err := flag.Set("test.benchtime", *benchtime); err != nil {
		log.Fatal(err)
	}

	p, err := loadPayload(*configFile, *size)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("== payload ==\nsource: %s\nraw json: %s\n\n", p.label, human(len(p.raw)))
	reportCodecs(p)
	reportDecoderLifecycle(p)

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr, DB: *redisDB, PoolSize: *concurrency * 2})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("ping redis at %s: %v", *redisAddr, err)
	}

	if !reportRedis(ctx, rdb, p, *players, *requests, *concurrency, *getsPerSec) {
		fmt.Println("\nRESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("\nRESULT: OK")
}

func loadPayload(file string, size int) (payload, error) {
	if file == "" {
		raw, err := json.Marshal(fakeconfig.Generate(size, 42))
		return payload{
			label:  fmt.Sprintf("synthetic foo/bar config (target %s, seed 42)", human(size)),
			raw:    raw,
			newOut: func() any { return &fakeconfig.Config{} },
		}, err
	}

	content, err := os.ReadFile(file)
	if err != nil {
		return payload{}, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		return payload{}, fmt.Errorf("%s is not valid JSON: %w", file, err)
	}
	return payload{
		label:  file + " (minified)",
		raw:    compact.Bytes(),
		newOut: func() any { return &map[string]any{} },
	}, nil
}

type codec struct {
	name   string
	encode func([]byte) []byte
	decode func([]byte) ([]byte, error)
}

func codecs() []codec {
	zstdLevel := func(name string, level zstd.EncoderLevel) codec {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
		if err != nil {
			log.Fatal(err)
		}
		return codec{
			name:   name,
			encode: func(raw []byte) []byte { return enc.EncodeAll(raw, nil) },
			decode: remoteconfig.Decompress,
		}
	}

	return []codec{
		{
			name: "deflate (stdlib default)",
			encode: func(raw []byte) []byte {
				var buf bytes.Buffer
				w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
				w.Write(raw)
				w.Close()
				return buf.Bytes()
			},
			decode: func(blob []byte) ([]byte, error) {
				return io.ReadAll(flate.NewReader(bytes.NewReader(blob)))
			},
		},
		zstdLevel("zstd fastest", zstd.SpeedFastest),
		{name: "zstd default  <- store", encode: remoteconfig.Compress, decode: remoteconfig.Decompress},
		zstdLevel("zstd better", zstd.SpeedBetterCompression),
		zstdLevel("zstd best", zstd.SpeedBestCompression),
	}
}

func reportCodecs(p payload) {
	unmarshal := func(raw []byte) {
		if err := json.Unmarshal(raw, p.newOut()); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println("== codecs (in-process, 1 goroutine, best of 3) ==")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "codec\tsize\tsaved\tencode\tdecode\tdecode+unmarshal\t")

	unmarshalOnly := bench(func() { unmarshal(p.raw) })
	fmt.Fprintf(tw, "none (today)\t%s\t-\t-\t-\t%s\t\n", human(len(p.raw)), micros(unmarshalOnly.NsPerOp()))

	for _, c := range codecs() {
		blob := c.encode(p.raw)
		decoded, err := c.decode(blob)
		if err != nil || !bytes.Equal(decoded, p.raw) {
			log.Fatalf("%s: round trip failed: %v", c.name, err)
		}

		enc := bench(func() { c.encode(p.raw) })
		dec := bench(func() { c.decode(blob) })
		decUnmarshal := bench(func() {
			raw, _ := c.decode(blob)
			unmarshal(raw)
		})
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t\n", c.name, human(len(blob)), saved(len(blob), len(p.raw)),
			micros(enc.NsPerOp()), micros(dec.NsPerOp()), micros(decUnmarshal.NsPerOp()))
	}
	tw.Flush()
	fmt.Println()
}

func reportDecoderLifecycle(p payload) {
	blob := remoteconfig.Compress(p.raw)

	shared := bench(func() { remoteconfig.Decompress(blob) })
	perRequest := bench(func() {
		d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		d.DecodeAll(blob, nil)
		d.Close()
	})

	fmt.Println("== zstd decoder lifecycle (1 goroutine, best of 3) ==")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "decoder\ttime/op\tB/op\tallocs/op\t")
	fmt.Fprintf(tw, "shared package-level (DecodeAll)\t%s\t%d\t%d\t\n", micros(shared.NsPerOp()), shared.AllocedBytesPerOp(), shared.AllocsPerOp())
	fmt.Fprintf(tw, "new decoder per request\t%s\t%d\t%d\t\n", micros(perRequest.NsPerOp()), perRequest.AllocedBytesPerOp(), perRequest.AllocsPerOp())
	tw.Flush()
	fmt.Println()
}

type loadResult struct {
	name      string
	ops       int
	bytesOut  float64
	redisCPU  time.Duration
	clientCPU time.Duration
	elapsed   time.Duration
	latencies []time.Duration
	sources   map[remoteconfig.Source]int
	errors    int
}

func reportRedis(ctx context.Context, rdb *redis.Client, p payload, players, requests, concurrency int, getsPerSec float64) bool {
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		log.Fatal(err)
	}

	writer := remoteconfig.NewStore(rdb, ttl, remoteconfig.WriteBoth, remoteconfig.ReadLegacy)
	var hash string
	for i := range players {
		h, err := writer.Store(ctx, playerID(i), json.RawMessage(p.raw))
		if err != nil {
			log.Fatal(err)
		}
		hash = h
	}

	legacyKey, zstdKey := remoteconfig.LegacyKey(hash), remoteconfig.ZstdKey(hash)
	legacyLen, zstdLen := rdb.StrLen(ctx, legacyKey).Val(), rdb.StrLen(ctx, zstdKey).Val()

	fmt.Printf("== redis keys (%d players seeded with WriteBoth, 1 config hash) ==\n", players)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "key\tSTRLEN\tMEMORY USAGE\t")
	fmt.Fprintf(tw, "%s\t%d\t%d\t\n", legacyKey, legacyLen, rdb.MemoryUsage(ctx, legacyKey).Val())
	fmt.Fprintf(tw, "%s\t%d\t%d\t\n", zstdKey, zstdLen, rdb.MemoryUsage(ctx, zstdKey).Val())
	tw.Flush()
	fmt.Println()

	ok := true
	expected := p.newOut()
	if err := json.Unmarshal(p.raw, expected); err != nil {
		log.Fatal(err)
	}

	runLoad(ctx, rdb, "warmup", requests/10, concurrency, func(rng *rand.Rand) (remoteconfig.Source, error) {
		return "", rdb.Get(ctx, remoteconfig.HashKey(playerID(rng.Intn(players)))).Err()
	})

	getBlob := func(name, key string) loadResult {
		return runLoad(ctx, rdb, name, requests, concurrency, func(*rand.Rand) (remoteconfig.Source, error) {
			return "", rdb.Get(ctx, key).Err()
		})
	}
	gets := []loadResult{getBlob("GET "+legacyKey, legacyKey), getBlob("GET "+zstdKey, zstdKey)}

	fmt.Printf("== redis only: GET of the blob key, no decode (%d GETs, %d concurrent) ==\n", requests, concurrency)
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "command\tbytes out\tper GET\tredis cpu/GET\tGETs/s\tp50\tp99\t")
	for _, r := range gets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f\t%s\t%s\t\n", r.name, human(int(r.bytesOut)), human(int(r.perOp(r.bytesOut))),
			micros(r.perOp(float64(r.redisCPU))), r.rate(), r.percentile(0.50), r.percentile(0.99))
	}
	tw.Flush()
	fmt.Printf("bytes out %s   redis cpu/GET %s   p50 %s\n\n",
		delta(gets[1].bytesOut, gets[0].bytesOut), delta(float64(gets[1].redisCPU), float64(gets[0].redisCPU)),
		delta(float64(gets[1].percentile(0.50)), float64(gets[0].percentile(0.50))))

	var retrieves []loadResult
	for _, mode := range []struct {
		name string
		read remoteconfig.ReadMode
		want remoteconfig.Source
	}{
		{"legacy (old pod)", remoteconfig.ReadLegacy, remoteconfig.SourceLegacy},
		{"zstd+fallback (new pod)", remoteconfig.ReadZstdFallbackLegacy, remoteconfig.SourceZstd},
	} {
		reader := remoteconfig.NewStore(rdb, ttl, remoteconfig.WriteBoth, mode.read)

		got := p.newOut()
		if _, err := reader.Retrieve(ctx, playerID(0), got); err != nil || !reflect.DeepEqual(got, expected) {
			fmt.Printf("FAIL %s: retrieved config differs from the stored one (err=%v)\n", mode.name, err)
			ok = false
		}

		r := runLoad(ctx, rdb, mode.name, requests, concurrency, func(rng *rand.Rand) (remoteconfig.Source, error) {
			return reader.Retrieve(ctx, playerID(rng.Intn(players)), p.newOut())
		})
		if r.errors > 0 || r.sources[mode.want] != requests {
			fmt.Printf("FAIL %s: want %d retrieves from %q, got %v (%d errors)\n", mode.name, requests, mode.want, r.sources, r.errors)
			ok = false
		}
		retrieves = append(retrieves, r)
	}

	fmt.Printf("== full retrieve: hash GET + blob GET + decode + json.Unmarshal (%d retrieves, %d concurrent) ==\n", requests, concurrency)
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "reader\tbytes out/retrieve\tpod cpu/retrieve\tredis cpu/retrieve\tretrieves/s\tp50\tp99\tserved from\t")
	for _, r := range retrieves {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f\t%s\t%s\t%v\t\n", r.name, human(int(r.perOp(r.bytesOut))),
			micros(r.perOp(float64(r.clientCPU))), micros(r.perOp(float64(r.redisCPU))), r.rate(),
			r.percentile(0.50), r.percentile(0.99), r.sources)
	}
	tw.Flush()
	legacy, compressed := retrieves[0], retrieves[1]
	fmt.Printf("bytes out %s   pod cpu %s   redis cpu %s\n",
		delta(compressed.bytesOut, legacy.bytesOut), delta(float64(compressed.clientCPU), float64(legacy.clientCPU)),
		delta(float64(compressed.redisCPU), float64(legacy.redisCPU)))
	if compressed.bytesOut >= legacy.bytesOut {
		fmt.Println("FAIL: compressed reader did not reduce bytes out")
		ok = false
	}

	const month = 30 * 24 * time.Hour
	monthly := func(blob int64) float64 { return float64(blob) * getsPerSec * month.Seconds() / 1e12 }
	fmt.Printf("\n== extrapolation: %.0f blob GETs/s for 30 days (blob STRLEN only) ==\n", getsPerSec)
	fmt.Printf("legacy %.1f TB/month -> zstd %.1f TB/month (saves %.1f TB/month)\n", monthly(legacyLen), monthly(zstdLen), monthly(legacyLen)-monthly(zstdLen))

	return ok
}

func runLoad(ctx context.Context, rdb *redis.Client, name string, ops, concurrency int, op func(*rand.Rand) (remoteconfig.Source, error)) loadResult {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
		r  = loadResult{name: name, ops: ops, sources: map[remoteconfig.Source]int{}}
	)

	before, cpuBefore, start := redisInfo(ctx, rdb), processCPU(), time.Now()
	for w := range concurrency {
		n := ops / concurrency
		if w < ops%concurrency {
			n++
		}
		wg.Add(1)
		go func(seed int64, n int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			latencies := make([]time.Duration, 0, n)
			sources := map[remoteconfig.Source]int{}
			errs := 0
			for range n {
				t := time.Now()
				source, err := op(rng)
				latencies = append(latencies, time.Since(t))
				if err != nil {
					errs++
					continue
				}
				sources[source]++
			}

			mu.Lock()
			defer mu.Unlock()
			r.latencies = append(r.latencies, latencies...)
			for s, c := range sources {
				r.sources[s] += c
			}
			r.errors += errs
		}(int64(w), n)
	}
	wg.Wait()
	r.elapsed, r.clientCPU = time.Since(start), processCPU()-cpuBefore
	after := redisInfo(ctx, rdb)

	r.bytesOut = after["total_net_output_bytes"] - before["total_net_output_bytes"]
	redisCPU := (after["used_cpu_user"] + after["used_cpu_sys"]) - (before["used_cpu_user"] + before["used_cpu_sys"])
	r.redisCPU = time.Duration(redisCPU * float64(time.Second))
	slices.Sort(r.latencies)
	return r
}

func (r loadResult) perOp(total float64) float64 { return total / float64(r.ops) }
func (r loadResult) rate() float64               { return float64(r.ops) / r.elapsed.Seconds() }

func (r loadResult) percentile(q float64) time.Duration {
	if len(r.latencies) == 0 {
		return 0
	}
	return r.latencies[int(q*float64(len(r.latencies)-1))].Round(time.Microsecond)
}

func redisInfo(ctx context.Context, rdb *redis.Client) map[string]float64 {
	info, err := rdb.Info(ctx, "stats", "cpu").Result()
	if err != nil {
		log.Fatal(err)
	}

	values := map[string]float64{}
	scanner := bufio.NewScanner(strings.NewReader(info))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		if !found {
			continue
		}
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			values[key] = f
		}
	}
	return values
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		log.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func bench(fn func()) testing.BenchmarkResult {
	var best testing.BenchmarkResult
	for range 3 {
		r := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fn()
			}
		})
		if best.N == 0 || r.NsPerOp() < best.NsPerOp() {
			best = r
		}
	}
	return best
}

func playerID(i int) string { return fmt.Sprintf("player-%06d", i) }

func micros[T int64 | float64](ns T) string {
	return fmt.Sprintf("%.1f µs", float64(ns)/1e3)
}

func human(n int) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1f KB", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func saved(compressed, raw int) string {
	return fmt.Sprintf("-%.1f%%", 100*(1-float64(compressed)/float64(raw)))
}

func delta(now, before float64) string {
	return fmt.Sprintf("%+.1f%%", 100*(now/before-1))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
