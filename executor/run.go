package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shamanec/gxcui/internal/xcodebuild"
	"github.com/shamanec/gxcui/reporter"
)

// EventType identifies a stage of a run, for progress reporting.
type EventType string

// Events emitted during a run.
const (
	EventResetStarted     EventType = "reset-started"
	EventResetFinished    EventType = "reset-finished"
	EventShutdownStarted  EventType = "shutdown-started"
	EventShutdownFinished EventType = "shutdown-finished"

	EventBootStarted   EventType = "boot-started"
	EventBootFinished  EventType = "boot-finished"
	EventBuildStarted  EventType = "build-started"
	EventBuildFinished EventType = "build-finished"
	EventEnumerated    EventType = "enumerated"
	EventPlanned       EventType = "planned"
	EventBatchStarted  EventType = "batch-started"
	EventBatchFinished EventType = "batch-finished"
	EventRetryStarted  EventType = "retry-started"
	EventReporting     EventType = "reporting"
)

// Event is a progress notification. Its fields are populated as they apply to
// the event type; callers should treat the zero value of any field as absent.
type Event struct {
	Type    EventType
	Message string

	Device  Device
	BatchID string
	Batch   *BatchResult

	// Completed and Total track batch progress within the current pass.
	Completed int
	Total     int
	// Attempt is the retry round, from 1.
	Attempt int
}

// ProgressFunc receives events as a run proceeds. It may be called from several
// goroutines at once and must be safe for concurrent use.
type ProgressFunc func(Event)

// RunOptions tunes a single run.
type RunOptions struct {
	// DryRun plans the run and prints what it would do without running tests.
	DryRun bool
	// Progress receives events as the run proceeds. Optional.
	Progress ProgressFunc
	// Output is where xcodebuild's own output goes when
	// execution.xcodebuildOutput is on — the build, the enumeration and every
	// batch, the way running xcodebuild by hand would show it. Batch lines are
	// tagged with the batch they came from, since batches run at the same time.
	//
	// Offering a writer does not by itself turn streaming on, and turning it on
	// without one produces nothing: the config decides whether, this decides
	// where. Per-batch log files are written either way, so this only affects
	// what the caller sees live.
	Output io.Writer
	// RunID overrides the generated run identifier, which is otherwise derived
	// from the start time.
	RunID string
	// Now overrides the clock, for tests.
	Now func() time.Time
}

func (o RunOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o RunOptions) emit(e Event) {
	if o.Progress != nil {
		o.Progress(e)
	}
}

// streamTo returns where xcodebuild's live output should go, or nil when this
// run streams none. Both halves have to be present: the configuration turns
// streaming on, and the caller says where it lands.
func (e *Executor) streamTo(w io.Writer) io.Writer {
	if w == nil || !e.cfg.Execution.XcodebuildOutput {
		return nil
	}
	return w
}

// Plan is the work a run intends to do, as produced by a dry run.
type RunPlan struct {
	XCTestRun string   `json:"xctestrun"`
	Devices   []Device `json:"devices"`
	Batches   []Batch  `json:"batches"`
	Strategy  Strategy `json:"strategy"`
	// Boot lists the simulators a real run would boot first. A dry run never
	// boots them itself.
	Boot []string `json:"boot,omitempty"`
	// Reset lists the simulators a real run would shut down and erase before
	// anything else, and Shutdown those it would shut down when it finishes.
	// Both are named in full, since a dry run is where anyone checks what a
	// destructive setting actually covers.
	Reset    []string `json:"reset,omitempty"`
	Shutdown []string `json:"shutdown,omitempty"`
	// Commands are the xcodebuild invocations, one per batch, in batch order.
	Commands []string `json:"commands"`
}

// runDirs are the directories one run writes into.
type runDirs struct {
	root    string
	batches string
	logs    string
}

func (d runDirs) create() error {
	for _, dir := range []string{d.root, d.batches, d.logs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create run directory: %w", err)
		}
	}
	return nil
}

// DryRun plans a run without executing any tests.
func (e *Executor) DryRun(ctx context.Context, opts RunOptions) (*RunPlan, error) {
	opts.DryRun = true
	prep, err := e.prepare(ctx, opts)
	if err != nil {
		return nil, err
	}

	plan := &RunPlan{
		XCTestRun: prep.project.XCTestRun,
		Devices:   prep.devices,
		Batches:   prep.batches,
		Strategy:  e.cfg.Batching.Strategy,
		Boot:      e.cfg.bootTargets(),
	}
	if e.cfg.Simulators.ResetBefore || e.cfg.Simulators.ShutdownAfter {
		scope, err := e.lifecycleScope(ctx)
		if err != nil {
			return nil, err
		}
		if e.cfg.Simulators.ResetBefore {
			plan.Reset = deviceNames(scope.Devices)
		}
		if e.cfg.Simulators.ShutdownAfter {
			plan.Shutdown = deviceNames(scope.Devices)
		}
	}
	for _, batch := range prep.batches {
		command, err := xcodebuild.TestCommand(xcodebuild.TestOptions{
			Project:          prep.project,
			Destination:      xcodebuild.SimulatorDestination(prep.devices[0].UDID),
			OnlyTesting:      batch.Tests,
			ResultBundlePath: filepath.Join(prep.dirs.batches, batch.ID+".xcresult"),
			TestTimeout:      e.cfg.Execution.TestTimeout,
			ExtraArgs:        e.cfg.Execution.ExtraArgs,
		})
		if err != nil {
			return nil, err
		}
		plan.Commands = append(plan.Commands, command)
	}
	return plan, nil
}

// preparation is everything resolved before the first test runs.
type preparation struct {
	project xcodebuild.Project
	devices []Device
	batches []Batch
	timings *reporter.Timings
	dirs    runDirs
	runID   string
	// stream carries xcodebuild's live output to RunOptions.Output. It is nil
	// when the caller did not ask for any.
	stream *lineStream
}

// prepare boots the configured simulators, then resolves devices, builds if
// needed, enumerates and plans batches.
func (e *Executor) prepare(ctx context.Context, opts RunOptions) (*preparation, error) {
	// Resetting and booting have to come first: a simulator that is still
	// coming up is not booted yet as far as the selection step is concerned. A
	// dry run is meant to be free of side effects, so it only reports what it
	// would do to the simulators.
	if !opts.DryRun {
		if err := e.resetSimulators(ctx, opts); err != nil {
			return nil, err
		}
		if err := e.bootSimulators(ctx, opts); err != nil {
			return nil, err
		}
	}

	selection, err := e.SelectDevices(ctx)
	if err != nil {
		return nil, err
	}
	if len(selection.Selected) == 0 {
		// A dry run deliberately skipped the boot, so the usual "boot one
		// yourself" advice would be wrong here.
		if opts.DryRun && len(e.cfg.bootTargets()) > 0 {
			return nil, fmt.Errorf("no eligible simulator: --dry-run does not boot, and a real run would boot %s first",
				e.cfg.bootList())
		}
		return nil, noEligibleDeviceError(selection)
	}
	devices := selection.Selected

	project, err := e.resolveProject(ctx, devices[0], opts)
	if err != nil {
		return nil, err
	}

	// The build and the enumeration are one process each, run before any batch
	// starts, so their output goes through untagged — exactly what the same
	// xcodebuild command would print.
	enumeration, err := e.enumerateProject(ctx, project, devices[0], e.streamTo(opts.Output))
	if err != nil {
		return nil, err
	}
	tests := enumeration.Tests()
	if len(tests) == 0 {
		return nil, emptySelectionError(enumeration)
	}
	opts.emit(Event{Type: EventEnumerated, Total: len(tests),
		Message: fmt.Sprintf("%d test(s) on %d simulator(s)", len(tests), len(devices))})

	timings, err := reporter.LoadTimings(e.cfg.Output.TimingsFile)
	if err != nil {
		return nil, err
	}

	batches, err := Plan(tests, BatchOptions{
		Strategy:   e.cfg.Batching.Strategy,
		Batches:    e.cfg.Batching.Batches,
		BatchSize:  e.cfg.Batching.BatchSize,
		Simulators: len(devices),
		Estimate:   timings.Estimate,
	})
	if err != nil {
		return nil, err
	}
	opts.emit(Event{Type: EventPlanned, Total: len(batches),
		Message: fmt.Sprintf("%d batch(es) across %d simulator(s)", len(batches), len(devices))})

	runID := opts.RunID
	if runID == "" {
		runID = opts.now().Format("20060102-150405")
	}
	root := filepath.Join(e.cfg.Output.Dir, runID)

	return &preparation{
		project: project,
		devices: devices,
		batches: batches,
		timings: timings,
		runID:   runID,
		stream:  newLineStream(e.streamTo(opts.Output)),
		dirs: runDirs{
			root:    root,
			batches: filepath.Join(root, "batches"),
			logs:    filepath.Join(root, "logs"),
		},
	}, nil
}

// resolveProject returns a project every batch can run from without building.
//
// Given a workspace or project, that means one build-for-testing up front; the
// resulting .xctestrun is then shared by every batch, which is what lets them
// run at the same time without contending on a build.
func (e *Executor) resolveProject(ctx context.Context, device Device, opts RunOptions) (xcodebuild.Project, error) {
	project := e.cfg.xcodebuildProject()
	if project.XCTestRun != "" || project.TestProducts != "" {
		return project, nil
	}

	if project.DerivedDataPath == "" {
		project.DerivedDataPath = filepath.Join(".gxcui", "dd")
	}
	opts.emit(Event{Type: EventBuildStarted, Device: device, Message: "building for testing"})

	xctestrun, err := xcodebuild.BuildForTesting(ctx, e.runner, xcodebuild.BuildOptions{
		Project:     project,
		Destination: xcodebuild.SimulatorDestination(device.UDID),
		ExtraArgs:   e.cfg.Execution.BuildArgs,
		Stdout:      e.streamTo(opts.Output),
		Stderr:      e.streamTo(opts.Output),
	})
	if err != nil {
		return xcodebuild.Project{}, err
	}
	opts.emit(Event{Type: EventBuildFinished, Message: xctestrun})

	// Everything downstream runs from the built product, not the source project.
	return xcodebuild.Project{
		XCTestRun:       xctestrun,
		DerivedDataPath: project.DerivedDataPath,
	}, nil
}

func emptySelectionError(e *Enumeration) error {
	var filtered int
	for _, plan := range e.Plans {
		filtered += len(plan.Filtered)
	}
	if filtered > 0 {
		return fmt.Errorf("no tests to run: tests.include/exclude dropped all %d of them", filtered)
	}
	return fmt.Errorf("no tests to run: xcodebuild reported none for this project")
}

// Run builds if needed, enumerates, batches and executes the tests, then writes
// the reports.
//
// Cancelling ctx stops scheduling new batches and kills the ones in flight, but
// Run still returns a result and writes reports from everything that finished:
// an interrupted run should not throw away the work it already did.
func (e *Executor) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	started := opts.now()

	prep, err := e.prepare(ctx, opts)
	if err != nil {
		// The simulators may already have been reset and booted, so release
		// them even though the run never got as far as a test.
		return nil, errors.Join(err, e.shutdownSimulators(ctx, opts))
	}
	if err := prep.dirs.create(); err != nil {
		return nil, errors.Join(err, e.shutdownSimulators(ctx, opts))
	}

	result := &RunResult{
		ID:        prep.runID,
		StartedAt: started,
		XCTestRun: prep.project.XCTestRun,
		Strategy:  e.cfg.Batching.Strategy,
		Devices:   prep.devices,
		Artifacts: Artifacts{Dir: prep.dirs.root, Logs: prep.dirs.logs},
	}

	outcomes := newOutcomeSet()
	pending := prep.batches

	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			opts.emit(Event{Type: EventRetryStarted, Attempt: attempt, Total: len(pending),
				Message: fmt.Sprintf("retrying %d test(s)", countTests(pending))})
		}

		batchResults := e.runPass(ctx, prep, pending, opts)
		result.Batches = append(result.Batches, batchResults...)
		for _, br := range batchResults {
			outcomes.record(br)
		}

		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}
		if attempt >= e.cfg.Retries.MaxAttempts {
			break
		}
		retryable := outcomes.retryable()
		if len(retryable) == 0 {
			break
		}
		pending = retryBatches(retryable, attempt+1, e.cfg.Retries.Isolate.Enabled())
	}

	result.Tests = outcomes.finish()
	result.Summary = summarize(result.Tests)
	result.FinishedAt = opts.now()
	result.Seconds = result.FinishedAt.Sub(result.StartedAt).Seconds()

	// The simulators are released as soon as the last batch is in: the reports
	// are built from result bundles on disk and need no simulator, so holding
	// several gigabytes of them through the slowest part of the run would be
	// pure waste. A failure here is reported but does not fail the run — the
	// tests have already had their say.
	shutdownErr := e.shutdownSimulators(ctx, opts)

	// Reports are written even for an interrupted run, from whatever completed.
	opts.emit(Event{Type: EventReporting, Message: "writing reports"})
	if err := e.report(ctx, prep, result, opts); err != nil {
		return result, errors.Join(err, shutdownErr)
	}
	return result, shutdownErr
}

// runPass executes one round of batches, one at a time per simulator.
func (e *Executor) runPass(ctx context.Context, prep *preparation, batches []Batch, opts RunOptions) []BatchResult {
	queue := make(chan Batch)
	go func() {
		defer close(queue)
		for _, batch := range batches {
			select {
			case queue <- batch:
			case <-ctx.Done():
				return
			}
		}
	}()

	var (
		mu        sync.Mutex
		results   []BatchResult
		completed int
	)

	var wg sync.WaitGroup
	for _, device := range prep.devices {
		wg.Add(1)
		go func(device Device) {
			defer wg.Done()
			for batch := range queue {
				// A UI test needs exclusive control of its simulator, so a
				// worker owns its device for the whole batch.
				opts.emit(Event{Type: EventBatchStarted, Device: device, BatchID: batch.ID,
					Total: len(batches), Completed: completedCount(&mu, &completed, 0)})

				br := e.runBatch(ctx, prep, batch, device, opts)

				mu.Lock()
				results = append(results, br)
				completed++
				done := completed
				mu.Unlock()

				opts.emit(Event{Type: EventBatchFinished, Device: device, BatchID: batch.ID,
					Batch: &br, Total: len(batches), Completed: done})
			}
		}(device)
	}
	wg.Wait()

	// Anything still queued when the run was cancelled never ran.
	if ctx.Err() != nil {
		ran := map[string]bool{}
		for _, r := range results {
			ran[r.ID] = true
		}
		for _, batch := range batches {
			if !ran[batch.ID] {
				results = append(results, BatchResult{
					ID: batch.ID, Hash: batch.Hash, Attempt: batch.Attempt,
					Tests: batch.Tests, Status: BatchCancelled,
					Unaccounted: batch.Tests,
				})
			}
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	return results
}

func completedCount(mu *sync.Mutex, counter *int, _ int) int {
	mu.Lock()
	defer mu.Unlock()
	return *counter
}

// runBatch runs one batch on one simulator and reads back what happened.
func (e *Executor) runBatch(ctx context.Context, prep *preparation, batch Batch, device Device, opts RunOptions) BatchResult {
	result := BatchResult{
		ID:        batch.ID,
		Hash:      batch.Hash,
		Attempt:   batch.Attempt,
		Device:    device,
		Tests:     batch.Tests,
		StartedAt: opts.now(),
	}

	bundlePath := filepath.Join(prep.dirs.batches, batch.ID+".xcresult")
	logPath := filepath.Join(prep.dirs.logs, batch.ID+".log")
	result.ResultBundle = bundlePath
	result.Log = logPath

	// xcodebuild refuses to write over an existing result bundle.
	if err := os.RemoveAll(bundlePath); err != nil {
		return finishBatch(result, opts, BatchNoResults, err)
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		return finishBatch(result, opts, BatchNoResults, err)
	}
	defer logFile.Close()

	// The log file always gets the full output; the stream is the caller's
	// live copy, tagged so that concurrent batches stay tellable apart.
	out := io.Writer(logFile)
	if tagged := prep.stream.writer("[" + batch.ID + "] "); tagged != nil {
		defer tagged.flush()
		out = io.MultiWriter(logFile, tagged)
	}

	batchCtx, cancel := context.WithTimeout(ctx, e.cfg.Execution.BatchTimeout.Duration())
	defer cancel()

	run, runErr := xcodebuild.RunTests(batchCtx, e.runner, xcodebuild.TestOptions{
		Project:          prep.project,
		Destination:      xcodebuild.SimulatorDestination(device.UDID),
		OnlyTesting:      batch.Tests,
		ResultBundlePath: bundlePath,
		TestTimeout:      e.cfg.Execution.TestTimeout,
		ExtraArgs:        e.cfg.Execution.ExtraArgs,
		Stdout:           out,
		Stderr:           out,
	})
	if run != nil {
		result.ExitCode = run.ExitCode
		result.Command = run.Command
	}

	switch {
	case runErr != nil && ctx.Err() != nil:
		return finishBatch(result, opts, BatchCancelled, nil)
	case runErr != nil && errors.Is(runErr, context.DeadlineExceeded):
		return finishBatch(result, opts, BatchTimedOut,
			fmt.Errorf("batch exceeded execution.batchTimeout (%s)", e.cfg.Execution.BatchTimeout))
	case runErr != nil:
		return finishBatch(result, opts, BatchNoResults, runErr)
	}

	// The exit code says where to look; the result bundle says what happened.
	// Only exit 65 carries information about the tests themselves, so anything
	// else is interpreted from the bundle, or treated as a lost batch when
	// there is no bundle to read.
	if _, err := os.Stat(bundlePath); err != nil {
		return finishBatch(result, opts, BatchNoResults,
			fmt.Errorf("xcodebuild exited %d without producing a result bundle; see %s", result.ExitCode, logPath))
	}

	cases, err := reporter.NewWithRunner(e.runner).ReadBundle(ctx, bundlePath)
	if err != nil {
		return finishBatch(result, opts, BatchNoResults, err)
	}
	result.cases = cases

	reported := map[string]bool{}
	for _, c := range cases {
		reported[c.Identifier] = true
		switch {
		case c.Result.Failed():
			result.Failed++
		case c.Result == reporter.ResultSkipped:
			result.Skipped++
		default:
			result.Passed++
		}
	}
	for _, test := range batch.Tests {
		if !reported[test] {
			result.Unaccounted = append(result.Unaccounted, test)
		}
	}

	return finishBatch(result, opts, BatchCompleted, nil)
}

func finishBatch(result BatchResult, opts RunOptions, status BatchStatus, err error) BatchResult {
	result.Status = status
	result.FinishedAt = opts.now()
	result.Seconds = result.FinishedAt.Sub(result.StartedAt).Seconds()
	if err != nil {
		result.Error = err.Error()
	}
	if status != BatchCompleted && len(result.Unaccounted) == 0 {
		// Nothing was reported, so every test in the batch is unaccounted for.
		result.Unaccounted = result.Tests
	}
	return result
}

// retryBatches turns failed tests into the next pass's work.
func retryBatches(tests []string, attempt int, isolate bool) []Batch {
	sort.Strings(tests)
	if !isolate {
		return []Batch{RetryBatch(0, attempt, tests)}
	}
	batches := make([]Batch, 0, len(tests))
	for i, test := range tests {
		batches = append(batches, RetryBatch(i, attempt, []string{test}))
	}
	return batches
}

func countTests(batches []Batch) int {
	var n int
	for _, b := range batches {
		n += b.Size()
	}
	return n
}

func summarize(tests []TestOutcome) Summary {
	var s Summary
	for _, t := range tests {
		s.Total++
		switch {
		case t.Result.Failed():
			s.Failed++
		case t.Result == reporter.ResultSkipped:
			s.Skipped++
		case t.Result == reporter.ResultUnknown:
			s.Unaccounted++
		default:
			s.Passed++
		}
		if t.Flaky {
			s.Flaky++
		}
	}
	return s
}
