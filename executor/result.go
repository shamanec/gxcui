package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/shamanec/gxcui/reporter"
)

// BatchStatus records how a batch invocation ended.
type BatchStatus string

const (
	// BatchCompleted means xcodebuild ran the batch and produced results,
	// whether or not the tests passed.
	BatchCompleted BatchStatus = "completed"
	// BatchNoResults means xcodebuild exited without leaving a readable result
	// bundle. The tests were not run, or their outcome is unknown, so they are
	// eligible to be tried again.
	BatchNoResults BatchStatus = "no-results"
	// BatchTimedOut means the batch exceeded execution.batchTimeout.
	BatchTimedOut BatchStatus = "timed-out"
	// BatchCancelled means the run was interrupted before the batch finished.
	BatchCancelled BatchStatus = "cancelled"
)

// BatchResult is what one batch invocation produced.
type BatchResult struct {
	ID      string `json:"id"`
	Hash    string `json:"hash"`
	Attempt int    `json:"attempt"`
	Device  Device `json:"device"`

	Tests []string `json:"tests"`

	Status   BatchStatus `json:"status"`
	ExitCode int         `json:"exitCode"`
	Command  string      `json:"command,omitempty"`
	Error    string      `json:"error,omitempty"`

	ResultBundle string `json:"resultBundle,omitempty"`
	Log          string `json:"log,omitempty"`

	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Seconds    float64   `json:"seconds"`

	// Passed, Failed and Skipped count the tests this invocation reported.
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Unaccounted lists tests the batch was asked to run that never appeared in
	// its results.
	Unaccounted []string `json:"unaccounted,omitempty"`

	// cases holds the parsed results of this invocation. It is not serialised:
	// the per-test record lives in RunResult.Tests, and the full detail stays in
	// the result bundle.
	cases []reporter.TestCase
}

// TestAttempt is one run of one test.
type TestAttempt struct {
	Attempt  int             `json:"attempt"`
	Batch    string          `json:"batch"`
	Device   string          `json:"device,omitempty"`
	Result   reporter.Result `json:"result"`
	Seconds  float64         `json:"seconds"`
	Failures []string        `json:"failures,omitempty"`
}

// TestOutcome is everything that happened to one test during a run.
type TestOutcome struct {
	Identifier string          `json:"identifier"`
	Result     reporter.Result `json:"result"`
	Seconds    float64         `json:"seconds"`
	// Flaky reports a test that failed at least once and then passed.
	Flaky    bool          `json:"flaky,omitempty"`
	Attempts []TestAttempt `json:"attempts"`
}

// LastAttempt returns the final attempt, or the zero value when there is none.
func (o TestOutcome) LastAttempt() TestAttempt {
	if len(o.Attempts) == 0 {
		return TestAttempt{}
	}
	return o.Attempts[len(o.Attempts)-1]
}

// Failures returns the failure messages of the final attempt.
func (o TestOutcome) Failures() []string { return o.LastAttempt().Failures }

// Device returns the device of the final attempt.
func (o TestOutcome) Device() string { return o.LastAttempt().Device }

// Summary counts the outcome of a run.
type Summary struct {
	Total       int `json:"total"`
	Passed      int `json:"passed"`
	Failed      int `json:"failed"`
	Skipped     int `json:"skipped"`
	Flaky       int `json:"flaky"`
	Unaccounted int `json:"unaccounted"`
}

// OK reports whether the run can be considered successful.
func (s Summary) OK() bool { return s.Failed == 0 && s.Unaccounted == 0 }

// Artifacts records where a run's outputs were written.
type Artifacts struct {
	Dir          string `json:"dir"`
	MergedBundle string `json:"mergedResultBundle,omitempty"`
	JUnit        string `json:"junit,omitempty"`
	HTML         string `json:"html,omitempty"`
	Manifest     string `json:"manifest,omitempty"`
	Timings      string `json:"timings,omitempty"`
	Logs         string `json:"logs,omitempty"`
}

// RunResult is the full record of a run, written to run.json.
type RunResult struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Seconds    float64   `json:"seconds"`

	// Interrupted reports that the run was cancelled before finishing. Reports
	// are still written from whatever completed.
	Interrupted bool `json:"interrupted,omitempty"`

	XCTestRun string   `json:"xctestrun,omitempty"`
	Strategy  Strategy `json:"strategy"`
	Devices   []Device `json:"devices"`

	Batches   []BatchResult `json:"batches"`
	Tests     []TestOutcome `json:"tests"`
	Summary   Summary       `json:"summary"`
	Artifacts Artifacts     `json:"artifacts"`
}

// FailedTests returns the outcomes that ended in failure, sorted by identifier.
func (r *RunResult) FailedTests() []TestOutcome {
	var failed []TestOutcome
	for _, t := range r.Tests {
		if t.Result.Failed() {
			failed = append(failed, t)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Identifier < failed[j].Identifier })
	return failed
}

// FlakyTests returns the tests that passed only after failing.
func (r *RunResult) FlakyTests() []TestOutcome {
	var flaky []TestOutcome
	for _, t := range r.Tests {
		if t.Flaky {
			flaky = append(flaky, t)
		}
	}
	sort.Slice(flaky, func(i, j int) bool { return flaky[i].Identifier < flaky[j].Identifier })
	return flaky
}

// UnaccountedTests returns tests whose outcome was never reported, usually
// because the batch running them died.
func (r *RunResult) UnaccountedTests() []TestOutcome {
	var unknown []TestOutcome
	for _, t := range r.Tests {
		if t.Result == reporter.ResultUnknown {
			unknown = append(unknown, t)
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Identifier < unknown[j].Identifier })
	return unknown
}

// RerunCommand renders a gxcui invocation that runs only the tests that did not
// pass, for pasting straight back into a terminal.
func (r *RunResult) RerunCommand() string {
	var ids []string
	for _, t := range append(r.FailedTests(), r.UnaccountedTests()...) {
		ids = append(ids, t.Identifier)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)

	cmd := "gxcui run"
	for _, id := range ids {
		cmd += fmt.Sprintf(" --include %q", id)
	}
	return cmd
}

// WriteManifest saves the run record as JSON.
func (r *RunResult) WriteManifest(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// LoadManifest reads a run record written by WriteManifest.
func LoadManifest(path string) (*RunResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var result RunResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return &result, nil
}
