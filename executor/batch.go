package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/shamanec/gxcui/internal/xcodebuild"
)

// Strategy selects how tests are distributed across batches.
type Strategy string

const (
	// StrategyDuration balances batches by predicted running time, using
	// recorded timings where available. It is the default: with no history it
	// degrades gracefully, and with history it is the only strategy that keeps
	// one slow class from becoming a straggler.
	StrategyDuration Strategy = "duration"
	// StrategyClass keeps every test of a class in the same batch, then balances
	// whole classes. Safest for suites where a class shares expensive set-up.
	StrategyClass Strategy = "class"
	// StrategyCount fills batches with a fixed number of tests, in order.
	StrategyCount Strategy = "count"
	// StrategyShard spreads tests round-robin over a fixed number of batches.
	StrategyShard Strategy = "shard"
)

// Strategies lists every supported strategy.
var Strategies = []Strategy{StrategyDuration, StrategyClass, StrategyCount, StrategyShard}

// Valid reports whether s is a strategy gxcui implements.
func (s Strategy) Valid() bool {
	for _, known := range Strategies {
		if s == known {
			return true
		}
	}
	return false
}

// Batch is one unit of work: the tests a single xcodebuild invocation runs on a
// single simulator.
type Batch struct {
	// ID is a stable, readable name used for the batch's result bundle and log
	// file, e.g. "batch-03". What a batch contains is in Tests, and in run.json:
	// naming it after one of its classes only reads as a promise that the batch
	// holds that class and nothing else, which no strategy but class keeps.
	ID string `json:"id"`
	// Index is the batch's position in the plan, from zero.
	Index int `json:"index"`
	// Attempt is 1 for the first run of these tests and rises with each retry.
	Attempt int `json:"attempt"`
	// Tests are the identifiers to run, in the order they were assigned.
	Tests []string `json:"tests"`
	// Hash identifies the batch by content, so the same set of tests is
	// recognisable across runs even when the index shifts.
	Hash string `json:"hash"`
	// EstimatedSeconds is the predicted running time used when planning.
	EstimatedSeconds float64 `json:"estimatedSeconds,omitempty"`
}

// Size returns the number of tests in the batch.
func (b Batch) Size() int { return len(b.Tests) }

// BatchOptions controls how tests are split.
type BatchOptions struct {
	Strategy Strategy
	// Batches is the number of batches to produce. Zero means auto: twice the
	// number of simulators.
	Batches int
	// BatchSize is the number of tests per batch, used by StrategyCount.
	BatchSize int
	// Simulators is how many simulators the batches will run on.
	Simulators int
	// Estimate predicts a test's duration in seconds. Nil weights every test
	// equally.
	Estimate func(identifier string) float64
}

// autoBatchesPerSimulator is how many batches each simulator gets by default.
//
// Deliberately more than one: a simulator that finishes a light batch takes the
// next one off the queue instead of idling, which absorbs the error in every
// duration estimate. One batch per simulator would make the whole run as slow as
// its unluckiest split.
const autoBatchesPerSimulator = 2

// Plan splits tests into batches.
//
// The result is deterministic: the same tests, options and estimates always
// produce the same batches in the same order, which keeps runs comparable and
// makes --dry-run meaningful.
func Plan(tests []string, opts BatchOptions) ([]Batch, error) {
	if len(tests) == 0 {
		return nil, fmt.Errorf("no tests to run")
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = StrategyDuration
	}
	if !strategy.Valid() {
		return nil, fmt.Errorf("unknown batching strategy %q: want one of %s", strategy, strategyNames())
	}

	estimate := opts.Estimate
	if estimate == nil {
		estimate = func(string) float64 { return 1 }
	}

	var groups [][]string
	switch strategy {
	case StrategyCount:
		groups = chunk(tests, batchSizeOrDefault(opts.BatchSize))
	case StrategyShard:
		groups = roundRobin(tests, batchCount(opts, len(tests)))
	case StrategyClass:
		groups = packGroups(groupByClass(tests), batchCount(opts, len(tests)), estimate)
	default: // StrategyDuration
		groups = packGroups(singletons(tests), batchCount(opts, len(tests)), estimate)
	}

	groups = enforceInvocationLimit(groups)
	return makeBatches(groups, estimate), nil
}

func strategyNames() string {
	names := make([]string, 0, len(Strategies))
	for _, s := range Strategies {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

func batchSizeOrDefault(size int) int {
	if size > 0 {
		return size
	}
	return 10
}

// batchCount resolves how many batches to produce, never more than there are
// tests to put in them.
func batchCount(opts BatchOptions, tests int) int {
	n := opts.Batches
	if n <= 0 {
		simulators := opts.Simulators
		if simulators <= 0 {
			simulators = 1
		}
		n = simulators * autoBatchesPerSimulator
	}
	if n > tests {
		n = tests
	}
	if n < 1 {
		n = 1
	}
	return n
}

// singletons wraps each test as its own group, for strategies that may split a
// class across batches.
func singletons(tests []string) [][]string {
	groups := make([][]string, 0, len(tests))
	for _, t := range tests {
		groups = append(groups, []string{t})
	}
	return groups
}

// groupByClass collects tests by their "Target/Class" prefix, preserving the
// order in which classes first appear.
func groupByClass(tests []string) [][]string {
	index := map[string]int{}
	var groups [][]string
	for _, t := range tests {
		key := classOf(t)
		i, ok := index[key]
		if !ok {
			index[key] = len(groups)
			groups = append(groups, []string{t})
			continue
		}
		groups[i] = append(groups[i], t)
	}
	return groups
}

// classOf returns the "Target/Class" prefix of a test identifier, or the whole
// identifier when it has no method component.
func classOf(identifier string) string {
	parts := strings.Split(identifier, "/")
	if len(parts) <= 1 {
		return identifier
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

// packGroups distributes groups over n bins using longest-processing-time-first:
// heaviest group into the emptiest bin, repeatedly. It is the standard greedy
// heuristic for this problem and beats round-robin whenever durations vary.
func packGroups(groups [][]string, n int, estimate func(string) float64) [][]string {
	if n > len(groups) {
		n = len(groups)
	}
	if n < 1 {
		n = 1
	}

	type weighted struct {
		tests  []string
		weight float64
		order  int
	}
	items := make([]weighted, 0, len(groups))
	for i, g := range groups {
		var w float64
		for _, t := range g {
			w += estimate(t)
		}
		items = append(items, weighted{tests: g, weight: w, order: i})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].weight != items[j].weight {
			return items[i].weight > items[j].weight
		}
		// Ties broken by original order so the plan is reproducible.
		return items[i].order < items[j].order
	})

	bins := make([][]string, n)
	loads := make([]float64, n)
	for _, item := range items {
		lightest := 0
		for i := 1; i < n; i++ {
			if loads[i] < loads[lightest] {
				lightest = i
			}
		}
		bins[lightest] = append(bins[lightest], item.tests...)
		loads[lightest] += item.weight
	}

	return dropEmpty(bins)
}

// chunk splits tests into consecutive runs of at most size.
func chunk(tests []string, size int) [][]string {
	var out [][]string
	for i := 0; i < len(tests); i += size {
		end := i + size
		if end > len(tests) {
			end = len(tests)
		}
		out = append(out, append([]string(nil), tests[i:end]...))
	}
	return out
}

// roundRobin deals tests across n bins like cards.
func roundRobin(tests []string, n int) [][]string {
	bins := make([][]string, n)
	for i, t := range tests {
		bins[i%n] = append(bins[i%n], t)
	}
	return dropEmpty(bins)
}

func dropEmpty(bins [][]string) [][]string {
	out := make([][]string, 0, len(bins))
	for _, b := range bins {
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// enforceInvocationLimit splits any group that would exceed the number of
// -only-testing arguments a single command can carry.
func enforceInvocationLimit(groups [][]string) [][]string {
	limit := xcodebuild.MaxTestsPerInvocation
	var out [][]string
	for _, g := range groups {
		if len(g) <= limit {
			out = append(out, g)
			continue
		}
		out = append(out, chunk(g, limit)...)
	}
	return out
}

func makeBatches(groups [][]string, estimate func(string) float64) []Batch {
	width := len(fmt.Sprint(len(groups)))
	if width < 2 {
		width = 2
	}

	batches := make([]Batch, 0, len(groups))
	for i, tests := range groups {
		var weight float64
		for _, t := range tests {
			weight += estimate(t)
		}
		batch := Batch{
			Index:            i,
			Attempt:          1,
			Tests:            tests,
			Hash:             hashTests(tests),
			EstimatedSeconds: weight,
		}
		batch.ID = batchID(i+1, width, 1)
		batches = append(batches, batch)
	}
	return batches
}

// RetryBatch builds a batch for a retry attempt of the given tests.
//
// Retries run one test per batch by default so that a test which only fails
// alongside a specific neighbour still gets a fair, isolated re-run.
func RetryBatch(index, attempt int, tests []string) Batch {
	return Batch{
		ID:      batchID(index+1, 2, attempt),
		Index:   index,
		Attempt: attempt,
		Tests:   tests,
		Hash:    hashTests(tests),
	}
}

// batchID renders the readable, sortable identity of a batch.
//
// Sequential rather than random: a UUID would be unsortable, unmemorable and
// different on every run, which makes logs and result bundles impossible to
// correlate. The number is zero-padded so that the bundles and logs sort in
// plan order in a directory listing.
func batchID(number, width, attempt int) string {
	id := fmt.Sprintf("batch-%0*d", width, number)
	if attempt > 1 {
		id += fmt.Sprintf("-try%d", attempt)
	}
	return id
}

// hashTests fingerprints a batch's contents, order-independently.
func hashTests(tests []string) string {
	sorted := append([]string(nil), tests...)
	sort.Strings(sorted)

	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])[:8]
}
