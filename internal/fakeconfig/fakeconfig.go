package fakeconfig

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
)

type Config struct {
	Version  string             `json:"version"`
	Features map[string]Feature `json:"features"`
}

type Feature struct {
	Enabled  bool           `json:"enabled"`
	Foo      string         `json:"foo"`
	Bar      int            `json:"bar"`
	StartsAt string         `json:"startsAt"`
	EndsAt   string         `json:"endsAt"`
	Segments []string       `json:"segments"`
	Params   map[string]int `json:"params"`
	Tiers    []Tier         `json:"tiers"`
}

type Tier struct {
	ID     string  `json:"id"`
	Level  int     `json:"level"`
	Cost   int     `json:"cost"`
	Weight float64 `json:"weight"`
	Reward Reward  `json:"reward"`
}

type Reward struct {
	Type   string `json:"type"`
	Amount int    `json:"amount"`
}

var (
	words = []string{
		"foo", "bar", "baz", "qux", "quux", "corge", "grault", "garply", "waldo", "fred", "plugh", "xyzzy", "thud",
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike",
		"november", "oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey", "xray", "yankee", "zulu",
	}
	rewardTypes = []string{"foo_coins", "bar_gems", "baz_chest", "qux_card"}
)

// Generate builds a deterministic foo/bar config whose minified JSON is at least targetBytes long.
func Generate(targetBytes int, seed int64) Config {
	rng := rand.New(rand.NewSource(seed))
	cfg := Config{Version: "foo-bar-1", Features: map[string]Feature{}}
	size := 0

	for i := 0; size < targetBytes; i++ {
		name := fmt.Sprintf("%s%s%02d", title(pick(rng, words)), title(pick(rng, words)), i)
		feature := newFeature(rng, i)
		cfg.Features[name] = feature

		raw, _ := json.Marshal(map[string]Feature{name: feature})
		size += len(raw)
	}

	return cfg
}

func newFeature(rng *rand.Rand, i int) Feature {
	tiers := make([]Tier, 3+rng.Intn(6))
	for level := range tiers {
		tiers[level] = Tier{
			ID:     fmt.Sprintf("%s.%08x", pick(rng, words), rng.Uint32()),
			Level:  level + 1,
			Cost:   (level + 1) * rng.Intn(5000),
			Weight: float64(rng.Intn(1000000)) / 1000000,
			Reward: Reward{
				Type:   rewardTypes[rng.Intn(len(rewardTypes))],
				Amount: (level + 1) * 10 * (1 + rng.Intn(10)),
			},
		}
	}

	segments := make([]string, 1+rng.Intn(3))
	for j := range segments {
		segments[j] = fmt.Sprintf("segment_%s_%d", words[rng.Intn(len(words))], rng.Intn(20))
	}

	paramCount := 2 + rng.Intn(6)
	params := make(map[string]int, paramCount)
	for range paramCount {
		params[title(pick(rng, words))+title(pick(rng, words))+title(pick(rng, words))] = rng.Intn(100000)
	}

	return Feature{
		Enabled:  rng.Intn(4) != 0,
		Foo:      strings.Repeat(words[i%len(words)]+"-", 1+rng.Intn(3)) + "bar",
		Bar:      rng.Intn(10000),
		StartsAt: fmt.Sprintf("2026-%02d-%02dT00:00:00Z", 1+rng.Intn(12), 1+rng.Intn(28)),
		EndsAt:   fmt.Sprintf("2026-%02d-%02dT23:59:59Z", 1+rng.Intn(12), 1+rng.Intn(28)),
		Segments: segments,
		Params:   params,
		Tiers:    tiers,
	}
}

func pick(rng *rand.Rand, from []string) string {
	return from[rng.Intn(len(from))]
}

func title(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}
