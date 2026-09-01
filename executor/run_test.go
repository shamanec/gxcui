package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shamanec/gxcui/internal/exec"
	"github.com/shamanec/gxcui/reporter"
)

// fakeXcode stands in for xcodebuild, simctl and xcresulttool at once, so a
// whole run can be exercised without Xcode. It dispatches on the command rather
// than replaying a fixed script, because batches run concurrently and their
// order is not deterministic.
type fakeXcode struct {
	t *testing.T

	devicesJSON string
	tests       []string

	// results maps a test to the outcome of each successive attempt. The last
	// entry repeats if a test runs more times than there are entries.
	results map[string][]reporter.Result
	// missing lists tests that never appear in a result bundle at all, standing
	// in for a batch that died halfway.
	missing map[string]bool

	mu       sync.Mutex
	attempts map[string]int
	bundles  map[string][]string
	commands []string
}

func newFakeXcode(t *testing.T, tests []string) *fakeXcode {
	return &fakeXcode{
		t:           t,
		devicesJSON: fixture(t, "devices.json"),
		tests:       tests,
		results:     map[string][]reporter.Result{},
		missing:     map[string]bool{},
		attempts:    map[string]int{},
		bundles:     map[string][]string{},
	}
}

func (f *fakeXcode) Run(ctx context.Context, cmd exec.Command) (*exec.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd.String())
	f.mu.Unlock()

	args := strings.Join(cmd.Args, " ")
	switch {
	case cmd.Name == "xcrun" && strings.Contains(args, "simctl"):
		return &exec.Result{Stdout: f.devicesJSON}, nil
	case cmd.Name == "xcodebuild" && strings.Contains(args, "-enumerate-tests"):
		return f.enumerate(cmd)
	case cmd.Name == "xcodebuild":
		return f.runTests(cmd)
	case cmd.Name == "xcrun" && strings.Contains(args, "test-results tests"):
		return f.readBundle(cmd)
	case cmd.Name == "xcrun" && strings.Contains(args, "test-results summary"):
		return f.readSummary(cmd)
	case cmd.Name == "xcrun" && strings.Contains(args, "test-results test-details"):
		return f.readDetails(cmd)
	case cmd.Name == "xcrun" && strings.Contains(args, "test-results activities"):
		return &exec.Result{Stdout: `{"testRuns":[]}`}, nil
	case cmd.Name == "xcrun" && strings.Contains(args, "export attachments"):
		return &exec.Result{}, os.MkdirAll(argValue(cmd.Args, "--output-path"), 0o755)
	case cmd.Name == "xcrun" && strings.Contains(args, "xcresulttool merge"):
		return f.merge(cmd)
	}
	f.t.Fatalf("unexpected command: %s", cmd)
	return nil, nil
}

func (f *fakeXcode) enumerate(cmd exec.Command) (*exec.Result, error) {
	var enabled []map[string]string
	for _, id := range f.tests {
		enabled = append(enabled, map[string]string{"identifier": id})
	}
	doc := map[string]any{
		"errors": []any{},
		"values": []map[string]any{{"testPlan": "Plan", "enabledTests": enabled, "disabledTests": []any{}}},
	}
	data, _ := json.Marshal(doc)
	return &exec.Result{}, os.WriteFile(argValue(cmd.Args, "-test-enumeration-output-path"), data, 0o644)
}

func (f *fakeXcode) runTests(cmd exec.Command) (*exec.Result, error) {
	bundle := argValue(cmd.Args, "-resultBundlePath")

	var requested []string
	for _, a := range cmd.Args {
		if rest, ok := strings.CutPrefix(a, "-only-testing:"); ok {
			requested = append(requested, rest)
		}
	}

	f.mu.Lock()
	f.bundles[bundle] = requested
	var failed bool
	for _, id := range requested {
		f.attempts[id]++
		if f.resultFor(id, f.attempts[id]).Failed() {
			failed = true
		}
	}
	f.mu.Unlock()

	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return nil, err
	}
	if failed {
		return &exec.Result{ExitCode: 65}, nil
	}
	return &exec.Result{}, nil
}

// resultFor returns the outcome of the nth attempt of a test, defaulting to
// passing when no script was given.
func (f *fakeXcode) resultFor(id string, attempt int) reporter.Result {
	scripted, ok := f.results[id]
	if !ok || len(scripted) == 0 {
		return reporter.ResultPassed
	}
	if attempt > len(scripted) {
		attempt = len(scripted)
	}
	return scripted[attempt-1]
}

func (f *fakeXcode) readBundle(cmd exec.Command) (*exec.Result, error) {
	bundle := argValue(cmd.Args, "--path")

	f.mu.Lock()
	requested := f.bundles[bundle]
	attempts := map[string]int{}
	for _, id := range requested {
		attempts[id] = f.attempts[id]
	}
	f.mu.Unlock()

	doc := reporter.Tests{
		Devices: []reporter.DeviceInfo{{ID: "udid", Name: "xcpool-1"}},
		TestNodes: []reporter.TestNode{{
			NodeType: "Test Plan", Name: "Plan",
			Children: []reporter.TestNode{{NodeType: "UI test bundle", Name: "App"}},
		}},
	}

	suites := map[string]int{}
	bundleNode := &doc.TestNodes[0].Children[0]
	for _, id := range requested {
		if f.missing[id] {
			continue
		}
		parts := strings.Split(id, "/")
		suiteName, testName := parts[1], parts[2]

		idx, ok := suites[suiteName]
		if !ok {
			bundleNode.Children = append(bundleNode.Children, reporter.TestNode{
				NodeType: "Test Suite", Name: suiteName,
			})
			idx = len(bundleNode.Children) - 1
			suites[suiteName] = idx
		}

		result := f.resultFor(id, attempts[id])
		node := reporter.TestNode{
			NodeType:          "Test Case",
			Name:              testName,
			NodeIdentifier:    suiteName + "/" + testName,
			Result:            result,
			DurationInSeconds: 1.5,
		}
		if result.Failed() {
			node.Children = []reporter.TestNode{{NodeType: "Failure Message", Name: testName + " went wrong"}}
		}
		bundleNode.Children[idx].Children = append(bundleNode.Children[idx].Children, node)
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &exec.Result{Stdout: string(data)}, nil
}

// merge records the merged bundle as covering every test its inputs did, so
// that reading it back afterwards behaves like a real merge.
func (f *fakeXcode) merge(cmd exec.Command) (*exec.Result, error) {
	output := argValue(cmd.Args, "--output-path")

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, arg := range cmd.Args {
		if requested, ok := f.bundles[arg]; ok {
			f.bundles[output] = append(f.bundles[output], requested...)
		}
	}
	return &exec.Result{}, os.MkdirAll(output, 0o755)
}

func (f *fakeXcode) readSummary(cmd exec.Command) (*exec.Result, error) {
	bundle := argValue(cmd.Args, "--path")

	f.mu.Lock()
	requested := f.bundles[bundle]
	summary := reporter.RunSummary{
		Title:      "Test - App",
		StartTime:  1786953946,
		FinishTime: 1786953951,
		Result:     reporter.ResultPassed,
	}
	for _, id := range requested {
		if f.missing[id] {
			continue
		}
		summary.TotalTestCount++
		if f.resultFor(id, f.attempts[id]).Failed() {
			summary.FailedTests++
			summary.Result = reporter.ResultFailed
		} else {
			summary.PassedTests++
		}
	}
	f.mu.Unlock()

	data, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	return &exec.Result{Stdout: string(data)}, nil
}

func (f *fakeXcode) readDetails(cmd exec.Command) (*exec.Result, error) {
	testID := argValue(cmd.Args, "--test-id")
	details := reporter.TestDetails{
		TestIdentifier: testID,
		TestResult:     reporter.ResultFailed,
		TestRuns: []reporter.TestNode{{
			NodeType: "Test Case Run", Name: testID + " went wrong", Result: reporter.ResultFailed,
			Children: []reporter.TestNode{{NodeType: "Source Code Reference", Name: "Tests.swift:42"}},
		}},
	}
	data, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	return &exec.Result{Stdout: string(data)}, nil
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runConfig(t *testing.T) Config {
	cfg := DefaultConfig()
	cfg.Project.XCTestRun = "App.xctestrun"
	cfg.Simulators.Include = []string{"xcpool-1", "xcpool-2"}
	cfg.Output.Dir = filepath.Join(t.TempDir(), "runs")
	cfg.Output.TimingsFile = filepath.Join(t.TempDir(), "timings.json")
	return cfg
}

// fixedClock returns a clock that advances a second per reading. Batches run
// concurrently and each one times itself, so the reading has to be guarded.
func fixedClock() func() time.Time {
	var mu sync.Mutex
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
}

func TestRunAllPassing(t *testing.T) {
	tests := []string{
		"App/AlphaTests/testOne()",
		"App/AlphaTests/testTwo()",
		"App/BetaTests/testThree()",
		"App/BetaTests/testFour()",
	}
	fake := newFakeXcode(t, tests)
	e := &Executor{cfg: ptr(runConfig(t)), runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !result.Summary.OK() {
		t.Errorf("Summary = %+v, want a clean run", result.Summary)
	}
	if result.Summary.Total != 4 || result.Summary.Passed != 4 {
		t.Errorf("Summary = %+v, want 4 total and 4 passed", result.Summary)
	}
	if result.RerunCommand() != "" {
		t.Errorf("RerunCommand() = %q, want empty when nothing failed", result.RerunCommand())
	}

	// Two simulators, so the default plan is four batches.
	if len(result.Batches) != 4 {
		t.Errorf("got %d batches, want 4", len(result.Batches))
	}
	for _, b := range result.Batches {
		if b.Status != BatchCompleted {
			t.Errorf("batch %s status = %q, want completed", b.ID, b.Status)
		}
	}

	if _, err := os.Stat(result.Artifacts.JUnit); err != nil {
		t.Errorf("no junit report written: %v", err)
	}
	if _, err := os.Stat(result.Artifacts.Manifest); err != nil {
		t.Errorf("no manifest written: %v", err)
	}
}

// A test that fails and then passes is reported as passed, but flagged flaky.
func TestRunRetriesFailuresAndMarksFlaky(t *testing.T) {
	tests := []string{"App/AlphaTests/testFlaky()", "App/AlphaTests/testSolid()"}
	fake := newFakeXcode(t, tests)
	fake.results["App/AlphaTests/testFlaky()"] = []reporter.Result{reporter.ResultFailed, reporter.ResultPassed}

	cfg := runConfig(t)
	cfg.Retries.MaxAttempts = 2
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !result.Summary.OK() {
		t.Errorf("Summary = %+v, want the retry to have rescued the run", result.Summary)
	}
	if result.Summary.Flaky != 1 {
		t.Errorf("Flaky = %d, want 1", result.Summary.Flaky)
	}

	flaky := result.FlakyTests()
	if len(flaky) != 1 || flaky[0].Identifier != "App/AlphaTests/testFlaky()" {
		t.Fatalf("FlakyTests() = %+v, want the flaky test", flaky)
	}
	if len(flaky[0].Attempts) != 2 {
		t.Errorf("got %d attempts, want 2", len(flaky[0].Attempts))
	}
	if flaky[0].Attempts[0].Result != reporter.ResultFailed {
		t.Errorf("first attempt = %q, want Failed", flaky[0].Attempts[0].Result)
	}

	// The retry runs the failing test on its own, not the whole batch again.
	var retry *BatchResult
	for i := range result.Batches {
		if result.Batches[i].Attempt == 2 {
			retry = &result.Batches[i]
		}
	}
	if retry == nil {
		t.Fatal("no retry batch was recorded")
	}
	if len(retry.Tests) != 1 {
		t.Errorf("retry batch ran %d tests, want just the failing one", len(retry.Tests))
	}
}

func TestRunReportsPersistentFailure(t *testing.T) {
	tests := []string{"App/AlphaTests/testBroken()", "App/AlphaTests/testFine()"}
	fake := newFakeXcode(t, tests)
	fake.results["App/AlphaTests/testBroken()"] = []reporter.Result{reporter.ResultFailed}

	cfg := runConfig(t)
	cfg.Retries.MaxAttempts = 2
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1", result.Summary.Failed)
	}
	if result.Summary.OK() {
		t.Error("Summary.OK() = true, want false when a test still fails")
	}

	failed := result.FailedTests()
	if len(failed) != 1 {
		t.Fatalf("FailedTests() = %+v, want one", failed)
	}
	if len(failed[0].Failures()) == 0 {
		t.Error("the failing test carries no failure message")
	}

	want := `gxcui run --include "App/AlphaTests/testBroken()"`
	if got := result.RerunCommand(); got != want {
		t.Errorf("RerunCommand() = %q, want %q", got, want)
	}
}

// A test the result bundle never mentions must stay visible rather than being
// silently dropped, and must be retried.
func TestRunTracksUnaccountedTests(t *testing.T) {
	tests := []string{"App/AlphaTests/testGhost()", "App/AlphaTests/testFine()"}
	fake := newFakeXcode(t, tests)
	fake.missing["App/AlphaTests/testGhost()"] = true

	cfg := runConfig(t)
	cfg.Retries.MaxAttempts = 2
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Summary.Unaccounted != 1 {
		t.Errorf("Unaccounted = %d, want 1: %+v", result.Summary.Unaccounted, result.Summary)
	}
	if result.Summary.OK() {
		t.Error("Summary.OK() = true, want false when a test never reported")
	}

	ghost := result.UnaccountedTests()
	if len(ghost) != 1 || ghost[0].Identifier != "App/AlphaTests/testGhost()" {
		t.Fatalf("UnaccountedTests() = %+v, want the missing test", ghost)
	}
	if len(ghost[0].Attempts) != 2 {
		t.Errorf("got %d attempts, want it retried", len(ghost[0].Attempts))
	}
	if !strings.Contains(result.RerunCommand(), "testGhost") {
		t.Errorf("RerunCommand() = %q, want it to include the unaccounted test", result.RerunCommand())
	}
}

func TestRunRecordsTimings(t *testing.T) {
	tests := []string{"App/AlphaTests/testOne()"}
	fake := newFakeXcode(t, tests)

	cfg := runConfig(t)
	e := &Executor{cfg: &cfg, runner: fake}

	if _, err := e.Run(context.Background(), RunOptions{Now: fixedClock()}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	timings, err := reporter.LoadTimings(cfg.Output.TimingsFile)
	if err != nil {
		t.Fatalf("LoadTimings() error = %v", err)
	}
	entry, ok := timings.Tests["App/AlphaTests/testOne()"]
	if !ok {
		t.Fatalf("no timing recorded, got %+v", timings.Tests)
	}
	if entry.Seconds != 1.5 {
		t.Errorf("Seconds = %v, want 1.5", entry.Seconds)
	}
}

// Cancelling must still produce a result and reports for whatever finished.
func TestRunInterrupted(t *testing.T) {
	tests := []string{"App/AlphaTests/testOne()", "App/AlphaTests/testTwo()"}
	fake := newFakeXcode(t, tests)

	cfg := runConfig(t)
	e := &Executor{cfg: &cfg, runner: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := e.Run(ctx, RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v, want a result despite cancellation", err)
	}
	if !result.Interrupted {
		t.Error("Interrupted = false, want true")
	}
	if _, err := os.Stat(result.Artifacts.Manifest); err != nil {
		t.Errorf("no manifest written for an interrupted run: %v", err)
	}
}

func TestDryRunRunsNoTests(t *testing.T) {
	tests := []string{"App/AlphaTests/testOne()", "App/BetaTests/testTwo()"}
	fake := newFakeXcode(t, tests)

	cfg := runConfig(t)
	e := &Executor{cfg: &cfg, runner: fake}

	plan, err := e.DryRun(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("DryRun() error = %v", err)
	}
	if len(plan.Batches) == 0 || len(plan.Commands) != len(plan.Batches) {
		t.Fatalf("plan has %d batches and %d commands", len(plan.Batches), len(plan.Commands))
	}
	for _, c := range plan.Commands {
		if !strings.Contains(c, "-only-testing:") || !strings.Contains(c, "-resultBundlePath") {
			t.Errorf("command is not a runnable batch invocation: %s", c)
		}
	}

	for _, cmd := range fake.commands {
		if strings.Contains(cmd, "-resultBundlePath") {
			t.Errorf("dry run executed a batch: %s", cmd)
		}
	}
}

func TestRunManifestRoundTrips(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()"})
	cfg := runConfig(t)
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	loaded, err := LoadManifest(result.Artifacts.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if loaded.ID != result.ID || loaded.Summary != result.Summary {
		t.Errorf("manifest round trip changed the run: %+v vs %+v", loaded.Summary, result.Summary)
	}
	if len(loaded.Tests) != len(result.Tests) {
		t.Errorf("manifest has %d tests, want %d", len(loaded.Tests), len(result.Tests))
	}
}

// The HTML report is built from the merged bundle, and gxcui adds to it what
// the bundle cannot know: that a test only passed on its second attempt.
func TestRunWritesHTMLReport(t *testing.T) {
	tests := []string{"App/AlphaTests/testFlaky()", "App/AlphaTests/testSolid()"}
	fake := newFakeXcode(t, tests)
	fake.results["App/AlphaTests/testFlaky()"] = []reporter.Result{reporter.ResultFailed, reporter.ResultPassed}

	cfg := runConfig(t)
	cfg.Retries.MaxAttempts = 2
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Artifacts.HTML == "" {
		t.Fatal("no HTML report was recorded in the artifacts")
	}
	data, err := os.ReadFile(result.Artifacts.HTML)
	if err != nil {
		t.Fatalf("read HTML report: %v", err)
	}

	html := string(data)
	for _, want := range []string{"<!DOCTYPE html>", "testFlaky()", "testSolid()", `class="badge flaky"`} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML report is missing %q", want)
		}
	}

	// The report must show the run's own elapsed time. A merged bundle's window
	// covers only its first input, so taking the duration from the bundle would
	// under-report a run whose batches came in waves.
	want := "⏱ " + reporterDuration(result.Seconds) + " elapsed"
	if !strings.Contains(html, want) {
		t.Errorf("HTML report does not show %q, so it is not using the run's own clock", want)
	}
}

// reporterDuration renders seconds the way the HTML report does.
func reporterDuration(seconds float64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%.1fs", seconds)
	default:
		return fmt.Sprintf("%dm %ds", int(seconds)/60, int(seconds)%60)
	}
}

// The report reads the per-batch bundles, so it no longer depends on the merge
// having happened — or having succeeded.
func TestRunWritesHTMLReportWithoutMerging(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()", "App/BetaTests/testTwo()"})

	cfg := runConfig(t)
	cfg.Output.Merge = Off()
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Artifacts.MergedBundle != "" {
		t.Errorf("MergedBundle = %q, want nothing when merging is off", result.Artifacts.MergedBundle)
	}
	if result.Artifacts.HTML == "" {
		t.Fatal("no HTML report was written, so it still depends on the merge")
	}
	data, err := os.ReadFile(result.Artifacts.HTML)
	if err != nil {
		t.Fatalf("read HTML report: %v", err)
	}
	for _, want := range []string{"testOne()", "testTwo()"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("HTML report is missing %q, so not every batch's bundle was read", want)
		}
	}
}

func TestRunSkipsHTMLReportWhenTurnedOff(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()"})

	cfg := runConfig(t)
	cfg.Output.HTML.Enabled = Off()
	e := &Executor{cfg: &cfg, runner: fake}

	result, err := e.Run(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Artifacts.HTML != "" {
		t.Errorf("Artifacts.HTML = %q, want nothing when the report is turned off", result.Artifacts.HTML)
	}
	for _, call := range fake.commands {
		if strings.Contains(call, "export attachments") {
			t.Errorf("attachments were exported for a report that was not wanted: %s", call)
		}
	}
}

func ptr[T any](v T) *T { return &v }
