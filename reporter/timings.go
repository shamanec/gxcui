package reporter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// TimingsVersion is the schema version of the timings file.
const TimingsVersion = 1

// Timings records how long each test took, so that a later run can balance its
// batches by predicted duration instead of by test count.
//
// Without history the batcher can only assume every test costs the same, which
// leaves the slow ones clumped together and one simulator running long after the
// others have finished.
type Timings struct {
	Version   int                   `json:"version"`
	UpdatedAt time.Time             `json:"updatedAt"`
	Tests     map[string]TestTiming `json:"tests"`
}

// TestTiming is the recorded duration of one test.
type TestTiming struct {
	// Seconds is an exponentially weighted moving average of observed
	// durations, so a single unusually slow run does not dominate the estimate
	// while genuine drift is still tracked.
	Seconds float64 `json:"seconds"`
	// Samples counts how many runs contributed.
	Samples int `json:"samples"`
	// UpdatedAt is when the test last ran.
	UpdatedAt time.Time `json:"updatedAt"`
}

// timingAlpha weights the newest observation in the moving average.
const timingAlpha = 0.3

// NewTimings returns an empty set.
func NewTimings() *Timings {
	return &Timings{Version: TimingsVersion, Tests: map[string]TestTiming{}}
}

// LoadTimings reads a timings file. A missing file is not an error: the first
// run has no history and simply starts collecting it.
func LoadTimings(path string) (*Timings, error) {
	if path == "" {
		return NewTimings(), nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewTimings(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read timings: %w", err)
	}

	t := NewTimings()
	if err := json.Unmarshal(data, t); err != nil {
		// A corrupt timings file is a cache miss, not a reason to fail a run.
		return NewTimings(), nil
	}
	if t.Tests == nil {
		t.Tests = map[string]TestTiming{}
	}
	if t.Version != TimingsVersion {
		return NewTimings(), nil
	}
	return t, nil
}

// Observe folds a measured duration into the history.
func (t *Timings) Observe(identifier string, seconds float64, now time.Time) {
	if identifier == "" || seconds <= 0 {
		return
	}
	if t.Tests == nil {
		t.Tests = map[string]TestTiming{}
	}

	entry, ok := t.Tests[identifier]
	switch {
	case !ok || entry.Seconds <= 0:
		entry = TestTiming{Seconds: seconds, Samples: 1}
	default:
		entry.Seconds = timingAlpha*seconds + (1-timingAlpha)*entry.Seconds
		entry.Samples++
	}
	entry.UpdatedAt = now
	t.Tests[identifier] = entry
}

// ObserveAll folds in every case that recorded a duration.
func (t *Timings) ObserveAll(cases []TestCase, now time.Time) {
	for _, c := range cases {
		t.Observe(c.Identifier, c.Duration, now)
	}
}

// Estimate returns the expected duration of a test. Tests with no history get
// the median of everything known, which is a far better guess than zero and
// keeps unknown tests from all landing in the same batch.
func (t *Timings) Estimate(identifier string) float64 {
	if t != nil {
		if entry, ok := t.Tests[identifier]; ok && entry.Seconds > 0 {
			return entry.Seconds
		}
	}
	return t.Median()
}

// Median returns the median recorded duration, or a neutral 1 second when there
// is no history at all.
func (t *Timings) Median() float64 {
	if t == nil || len(t.Tests) == 0 {
		return 1
	}
	values := make([]float64, 0, len(t.Tests))
	for _, entry := range t.Tests {
		if entry.Seconds > 0 {
			values = append(values, entry.Seconds)
		}
	}
	if len(values) == 0 {
		return 1
	}
	sort.Float64s(values)
	return values[len(values)/2]
}

// Save writes the timings file, creating parent directories as needed.
//
// Keys are written in sorted order so the file diffs cleanly for teams that
// commit it to share balancing data across CI machines.
func (t *Timings) Save(path string, now time.Time) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write timings: %w", err)
	}

	t.Version = TimingsVersion
	t.UpdatedAt = now

	// json.Marshal sorts map keys, so the output is already stable; this just
	// makes the guarantee explicit and independent of that implementation
	// detail.
	ordered := make(map[string]TestTiming, len(t.Tests))
	for k, v := range t.Tests {
		ordered[k] = v
	}
	t.Tests = ordered

	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("write timings: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write timings: %w", err)
	}
	return nil
}

// Slowest returns the n slowest known tests, longest first.
func (t *Timings) Slowest(n int) []struct {
	Identifier string
	Seconds    float64
} {
	type entry struct {
		Identifier string
		Seconds    float64
	}
	all := make([]entry, 0, len(t.Tests))
	for id, timing := range t.Tests {
		all = append(all, entry{id, timing.Seconds})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Seconds != all[j].Seconds {
			return all[i].Seconds > all[j].Seconds
		}
		return all[i].Identifier < all[j].Identifier
	})
	if n > 0 && len(all) > n {
		all = all[:n]
	}

	out := make([]struct {
		Identifier string
		Seconds    float64
	}, len(all))
	for i, e := range all {
		out[i].Identifier = e.Identifier
		out[i].Seconds = e.Seconds
	}
	return out
}
