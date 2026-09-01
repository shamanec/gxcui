package reporter

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Detail says how much of an optional, expensive extra to collect.
//
// Both activity logs and attachments cost an xcresulttool call per test and can
// dominate both report-generation time and file size, so each is chosen
// separately.
type Detail string

// Detail levels.
const (
	// DetailNone collects nothing.
	DetailNone Detail = "none"
	// DetailFailed collects only for tests that failed, which is the level worth
	// paying for: nobody reads the screenshots of a passing test.
	DetailFailed Detail = "failed"
	// DetailAll collects for every test.
	DetailAll Detail = "all"
)

// Valid reports whether d is a known level.
func (d Detail) Valid() bool {
	switch d {
	case DetailNone, DetailFailed, DetailAll:
		return true
	}
	return false
}

// wants reports whether a test with the given result needs this detail.
func (d Detail) wants(r Result) bool {
	switch d {
	case DetailAll:
		return true
	case DetailFailed:
		return r.Failed()
	default:
		return false
	}
}

// DetailNames lists the valid Detail values, for error messages.
func DetailNames() string { return "none, failed or all" }

// HTMLOptions tunes HTML report generation.
type HTMLOptions struct {
	// Title overrides the run title taken from the result bundle.
	Title string
	// StartTime and FinishTime override the run window recorded in the bundle.
	//
	// They exist because a merged bundle's window is wrong: xcresulttool merge
	// keeps the first input's timings rather than the union of them all, so a
	// run whose batches came in waves reports only its first wave. Whatever
	// scheduled the batches is the only thing that knows when the run really
	// started and finished.
	StartTime  time.Time
	FinishTime time.Time
	// Activities selects which tests get their step-by-step log included.
	Activities Detail
	// Attachments selects which tests get their screenshots and recordings
	// embedded. Attachments are embedded as data URIs, so the report stays a
	// single file that survives being emailed or archived.
	Attachments Detail
	// MaxAttachmentBytes drops any single attachment larger than this. Zero
	// means no limit.
	MaxAttachmentBytes int64
	// Coverage includes the run's line coverage. It costs one pass over every
	// bundle and adds nothing when the scheme did not gather coverage, so it is
	// off unless asked for.
	Coverage bool
	// Attempts maps a full test identifier to how many times it ran. gxcui
	// knows this and the result bundle does not.
	Attempts map[string]int
	// Flaky marks tests that passed only after failing.
	Flaky map[string]bool
	// Generator labels the report's footer, e.g. "gxcui 1.2.0".
	Generator string
}

func (o *HTMLOptions) applyDefaults() {
	if o.Activities == "" {
		o.Activities = DetailFailed
	}
	if o.Attachments == "" {
		o.Attachments = DetailFailed
	}
}

// HTMLReport is the model the HTML template renders. It is deliberately kept
// apart from the xcresulttool schema types so that neither the template nor the
// parser has to change when the other does.
type HTMLReport struct {
	Title       string
	Environment string
	StartTime   time.Time
	FinishTime  time.Time
	// Seconds is the run's wall-clock duration.
	Seconds float64
	// TestSeconds is the total time the tests themselves took, summed. Running
	// on several simulators at once makes it larger than Seconds, and the gap
	// between the two is what the parallelism bought.
	TestSeconds float64
	Result      Result
	Counts      HTMLCounts
	Devices     []DeviceInfo
	Suites      []HTMLSuite
	Generator   string
	GeneratedAt time.Time
	// Coverage is the run's line coverage, or nil when it was not asked for or
	// the bundles carry none.
	Coverage *HTMLCoverage
}

// HTMLCounts holds the run's headline figures.
type HTMLCounts struct {
	Total            int
	Passed           int
	Failed           int
	Skipped          int
	ExpectedFailures int
	Flaky            int
}

// HTMLSuite is one test bundle.
type HTMLSuite struct {
	Name    string
	Seconds float64
	Result  Result
	Classes []HTMLClass
}

// HTMLClass is one test class within a bundle.
type HTMLClass struct {
	Name    string
	Seconds float64
	Result  Result
	Tests   []HTMLTest
}

// HTMLTest is one execution of one test method.
//
// A test that was retried on a different simulator appears once per execution,
// which is what you want when the question is "why did it pass the second time".
type HTMLTest struct {
	Name string
	// Identifier is the full "Target/Class/method()" form.
	Identifier string
	// NodeID is the identifier xcresulttool uses, which omits the target.
	NodeID  string
	Seconds float64
	Result  Result
	Device  string

	// Attempts is how many times gxcui ran this test, and Flaky marks one that
	// only passed after failing. Both are zero-valued when the report is built
	// from a bundle alone.
	Attempts int
	Flaky    bool

	FailureMessage string
	SourceLocation string
	Failures       []Failure

	Activities  []HTMLActivity
	Attachments []HTMLAttachment
}

// ShowTestTime reports whether the total test time is worth showing beside the
// elapsed time. It is not when the run used one simulator, since the two
// figures are then nearly the same and the second only adds noise.
func (r *HTMLReport) ShowTestTime() bool {
	return len(r.Devices) > 1 && r.TestSeconds > r.Seconds
}

// ShowDevices reports whether each test should say which simulator ran it. On a
// one-simulator run the answer is the same for every test and is already in the
// header, so the badge would be pure noise.
func (r *HTMLReport) ShowDevices() bool { return len(r.Devices) > 1 }

// Retried reports whether the test ran more than once.
func (t HTMLTest) Retried() bool { return t.Attempts > 1 }

// HTMLActivity is one step in a test's activity log.
type HTMLActivity struct {
	Title       string
	StartTime   time.Time
	Type        string
	Attachments []HTMLAttachment
	Children    []HTMLActivity
}

// HTMLAttachment is a screenshot, recording or log embedded in the report.
type HTMLAttachment struct {
	Name     string
	MIMEType string
	// Data is the base64-encoded file contents. Images use it as a data URI;
	// recordings are turned into a blob at load time, since Safari will not play
	// a video from a data: URI.
	Data string
	// Text holds a text attachment's contents, shown inline in place of Data.
	Text string
	// Truncated marks a text attachment that was cut short.
	Truncated bool
	// Omitted marks an attachment the size limit kept out. The report still
	// lists it, because silently dropping a screen recording looks exactly like
	// a bug in the report.
	Omitted bool
	// Bytes is the file's size on disk.
	Bytes int64

	// file is the exported file's name, which is what tells two attachments
	// apart when their contents or labels coincide.
	file string
}

// WriteHTML generates a self-contained HTML report for a result bundle and
// writes it to outputPath.
//
// Everything the report needs — CSS, screenshots, recordings — is inlined, so
// the file works on its own with no directory of assets beside it.
func (r *Reporter) WriteHTML(ctx context.Context, bundlePath, outputPath string, opts HTMLOptions) error {
	return r.WriteHTMLFromBundles(ctx, []string{bundlePath}, outputPath, opts)
}

// WriteHTMLFromBundles generates one HTML report covering several result
// bundles and writes it to outputPath.
//
// Reading the per-batch bundles directly is preferable to merging them first
// and reporting on the merge. `xcresulttool merge` keeps only its first input's
// timings, so a merged bundle mis-states when the run started and finished,
// and it discards which bundle — and therefore which simulator — each result
// came from. Both survive when the bundles are read as they are.
func (r *Reporter) WriteHTMLFromBundles(ctx context.Context, bundlePaths []string, outputPath string, opts HTMLOptions) error {
	report, err := r.BuildHTMLFromBundles(ctx, bundlePaths, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("write html report: %w", err)
	}
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("write html report: %w", err)
	}
	defer f.Close()

	if err := RenderHTML(f, report); err != nil {
		return err
	}
	return f.Close()
}

// BuildHTML assembles the report model for a single result bundle.
func (r *Reporter) BuildHTML(ctx context.Context, bundlePath string, opts HTMLOptions) (*HTMLReport, error) {
	return r.BuildHTMLFromBundles(ctx, []string{bundlePath}, opts)
}

// BuildHTMLFromBundles assembles one report model covering several bundles.
//
// Each bundle costs two xcresulttool calls plus one per test for whichever
// details the options ask for. The per-test calls dominate, so they run
// concurrently; the bundles themselves are read in sequence so that a run with
// many batches does not multiply the concurrency by the batch count.
func (r *Reporter) BuildHTMLFromBundles(ctx context.Context, bundlePaths []string, opts HTMLOptions) (*HTMLReport, error) {
	opts.applyDefaults()
	if len(bundlePaths) == 0 {
		return nil, fmt.Errorf("build html report: no result bundles given")
	}
	if !opts.Activities.Valid() {
		return nil, fmt.Errorf("html activities %q is not %s", opts.Activities, DetailNames())
	}
	if !opts.Attachments.Valid() {
		return nil, fmt.Errorf("html attachments %q is not %s", opts.Attachments, DetailNames())
	}

	bundles := make([]*bundleData, 0, len(bundlePaths))
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	for _, path := range bundlePaths {
		bundle, cleanup, err := r.readBundleData(ctx, path, opts)
		cleanups = append(cleanups, cleanup)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}

	report := combine(bundles, opts)
	if opts.Coverage {
		// Coverage is an extra, not the point of the report. A scheme that
		// never gathered any is the ordinary case rather than an error, and
		// losing the whole report because xccov was unhappy would be a poor
		// trade for a section nobody had before — so the section is dropped and
		// the results still get written.
		if coverage, err := r.ReadCoverage(ctx, bundlePaths); err == nil {
			report.Coverage = htmlCoverage(coverage)
		}
	}
	return report, nil
}

// bundleData is everything read out of one result bundle.
type bundleData struct {
	path        string
	summary     *RunSummary
	tests       *Tests
	device      DeviceInfo
	details     map[string]*TestDetails
	activities  map[string][]ActivityNode
	attachments *attachmentSet
}

func (r *Reporter) readBundleData(ctx context.Context, path string, opts HTMLOptions) (*bundleData, func(), error) {
	noop := func() {}

	summary, err := r.ReadSummary(ctx, path)
	if err != nil {
		return nil, noop, err
	}
	tests, err := r.ReadRawTests(ctx, path)
	if err != nil {
		return nil, noop, err
	}

	leaves := leafTests(tests)
	details, activities := r.fetchPerTest(ctx, path, leaves, opts)

	attachments, cleanup, err := r.collectAttachments(ctx, path, opts, wantedTests(leaves, opts))
	if err != nil {
		// A report without screenshots still tells you what failed, so this is
		// not worth aborting for.
		attachments = nil
	}

	bundle := &bundleData{
		path:        path,
		summary:     summary,
		tests:       tests,
		details:     details,
		activities:  activities,
		attachments: attachments,
	}
	// A per-batch bundle ran on exactly one simulator and does not repeat that
	// on every test node, so it is picked up here and attached to the rows.
	if devices := summary.devices(tests); len(devices) == 1 {
		bundle.device = devices[0]
	}
	return bundle, cleanup, nil
}

// leafTest is a Test Case node picked out of the tree, before the full model is
// built.
type leafTest struct {
	nodeID string
	result Result
}

// wantedTests returns the tests whose attachments belong in the report, or nil
// to mean "every test".
func wantedTests(leaves []leafTest, opts HTMLOptions) map[string]bool {
	if opts.Attachments != DetailFailed {
		return nil
	}
	wanted := map[string]bool{}
	for _, leaf := range leaves {
		if leaf.result.Failed() {
			wanted[leaf.nodeID] = true
		}
	}
	return wanted
}

func leafTests(tests *Tests) []leafTest {
	var out []leafTest
	seen := map[string]bool{}
	var walkNode func(n *TestNode)
	walkNode = func(n *TestNode) {
		if n.NodeType == nodeTypeTestCase {
			if n.NodeIdentifier != "" && !seen[n.NodeIdentifier] {
				seen[n.NodeIdentifier] = true
				out = append(out, leafTest{nodeID: n.NodeIdentifier, result: normalizeResult(n.Result)})
			}
			return
		}
		for i := range n.Children {
			walkNode(&n.Children[i])
		}
	}
	for i := range tests.TestNodes {
		walkNode(&tests.TestNodes[i])
	}
	return out
}

// fetchPerTest retrieves the details and activity logs the options call for.
//
// xcresulttool spends its time reading the bundle's database rather than on the
// CPU, so a handful of concurrent calls is a large win; too many is not, and
// costs a process each.
func (r *Reporter) fetchPerTest(ctx context.Context, bundlePath string, leaves []leafTest, opts HTMLOptions) (map[string]*TestDetails, map[string][]ActivityNode) {
	details := map[string]*TestDetails{}
	activities := map[string][]ActivityNode{}

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, leaf := range leaves {
		// Details only add a source location to a failure, so passing tests
		// gain nothing from the call.
		wantDetails := leaf.result.Failed()
		wantActivities := opts.Activities.wants(leaf.result)
		if !wantDetails && !wantActivities {
			continue
		}

		wg.Add(1)
		go func(leaf leafTest, wantDetails, wantActivities bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if wantDetails {
				if d, err := r.ReadTestDetails(ctx, bundlePath, leaf.nodeID); err == nil {
					mu.Lock()
					details[leaf.nodeID] = d
					mu.Unlock()
				}
			}
			if wantActivities {
				if a, err := r.ReadActivities(ctx, bundlePath, leaf.nodeID); err == nil && len(a.TestRuns) > 0 {
					// The last run is the one whose result the report shows.
					mu.Lock()
					activities[leaf.nodeID] = a.TestRuns[len(a.TestRuns)-1].Activities
					mu.Unlock()
				}
			}
		}(leaf, wantDetails, wantActivities)
	}

	wg.Wait()
	return details, activities
}

// attachmentSet holds the exported attachment files, indexed the two ways the
// report needs to find them.
type attachmentSet struct {
	dir string
	// byUUID maps an activity attachment's UUID to its exported file name.
	byUUID map[string]ManifestAttachment
	// byTest maps a test's node identifier to everything exported for it, in
	// the order the manifest lists them.
	byTest map[string][]ManifestAttachment
	limit  int64
}

// collectAttachments exports the bundle's attachments to a temporary directory
// and indexes them, keeping only the tests in wanted — nil meaning all of them.
// The returned cleanup removes the directory.
//
// The export is never narrowed with `--only-failures`, even when just the failed
// tests are wanted. That flag selects attachments Xcode flagged as belonging to
// a failure — the final UI snapshot and element dump — and a screen recording is
// not one of them, so the flag drops the very thing a reader opens the report
// for. Exporting everything and then keeping the failing tests' files costs
// temporary disk in exchange for a report that actually holds the recording.
func (r *Reporter) collectAttachments(ctx context.Context, bundlePath string, opts HTMLOptions, wanted map[string]bool) (*attachmentSet, func(), error) {
	noop := func() {}
	if opts.Attachments == DetailNone {
		return nil, noop, nil
	}

	dir, err := os.MkdirTemp("", "gxcui-attachments-*")
	if err != nil {
		return nil, noop, fmt.Errorf("export attachments: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	if err := r.ExportAttachments(ctx, bundlePath, dir, false); err != nil {
		cleanup()
		return nil, noop, err
	}

	set, err := indexManifest(dir, opts.MaxAttachmentBytes, wanted)
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	return set, cleanup, nil
}

// indexManifest reads an export directory's manifest and indexes the
// attachments belonging to the wanted tests — nil meaning all of them.
func indexManifest(dir string, limit int64, wanted map[string]bool) (*attachmentSet, error) {
	manifest, err := readAttachmentManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}

	set := &attachmentSet{
		dir:    dir,
		byUUID: map[string]ManifestAttachment{},
		byTest: map[string][]ManifestAttachment{},
		limit:  limit,
	}
	for _, test := range manifest {
		for _, att := range test.Attachments {
			// A test identifier the tree does not know is no reason to drop an
			// attachment Xcode itself tied to a failure.
			if wanted != nil && !wanted[test.TestIdentifier] && !att.IsAssociatedWithFailure {
				continue
			}
			set.byTest[test.TestIdentifier] = append(set.byTest[test.TestIdentifier], att)
			// xcresulttool names exported files after the attachment's UUID,
			// which is how an activity log entry finds its own file. The name
			// is not guaranteed, so the per-test list is the fallback.
			if uuid := strings.TrimSuffix(att.ExportedFileName, filepath.Ext(att.ExportedFileName)); uuid != "" {
				set.byUUID[uuid] = att
			}
		}
	}
	return set, nil
}

func readAttachmentManifest(path string) (AttachmentManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A run with no attachments at all writes no manifest.
			return nil, nil
		}
		return nil, fmt.Errorf("read attachment manifest: %w", err)
	}
	var manifest AttachmentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse attachment manifest: %w", err)
	}
	return manifest, nil
}

// maxTextBytes caps how much of a text attachment the report inlines. A UI
// hierarchy dump runs to megabytes, and the first pages are the ones read.
const maxTextBytes = 64 << 10

// load reads an exported attachment and returns it ready to embed. It reports
// false for anything missing or unrenderable, so an unreadable screenshot costs
// a screenshot and not the report.
func (s *attachmentSet) load(att ManifestAttachment) (HTMLAttachment, bool) {
	if s == nil || att.ExportedFileName == "" {
		return HTMLAttachment{}, false
	}
	path := filepath.Join(s.dir, att.ExportedFileName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return HTMLAttachment{}, false
	}
	name := att.SuggestedHumanReadableName
	if name == "" {
		name = att.ExportedFileName
	}
	loaded := HTMLAttachment{Name: name, Bytes: info.Size(), file: att.ExportedFileName}

	if s.limit > 0 && info.Size() > s.limit {
		// Without the bytes there is nothing to sniff, so fall back to the name.
		loaded.MIMEType = namedMIME(att.ExportedFileName, name)
		loaded.Omitted = true
		return loaded, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return HTMLAttachment{}, false
	}

	loaded.MIMEType = detectMIME(data, att.ExportedFileName, name)
	switch {
	case loaded.MIMEType == mimeAppleArchive:
		// A serialised XCUIElement tree: XCTest's own bookkeeping, with nothing
		// a browser can show and tens of kilobytes per screenshot-sized step.
		return HTMLAttachment{}, false
	case strings.HasPrefix(loaded.MIMEType, "text/"):
		if len(data) > maxTextBytes {
			data, loaded.Truncated = data[:maxTextBytes], true
		}
		loaded.Text = string(data)
	default:
		loaded.Data = base64.StdEncoding.EncodeToString(data)
	}
	return loaded, true
}

// forActivity returns the file captured by one activity step, matching on the
// UUID the activity log records, then on the name, then on capture time.
func (s *attachmentSet) forActivity(nodeID string, att ActivityAttachment) (HTMLAttachment, bool) {
	if s == nil {
		return HTMLAttachment{}, false
	}
	if manifest, ok := s.byUUID[att.UUID]; ok {
		return s.load(manifest)
	}
	// Several attachments of one step share a timestamp — the failure snapshot
	// and the element dump are written together — so the name is tried first.
	for _, candidate := range s.byTest[nodeID] {
		if att.Name != "" && candidate.SuggestedHumanReadableName == att.Name {
			return s.load(candidate)
		}
	}
	for _, candidate := range s.byTest[nodeID] {
		if math.Abs(candidate.Timestamp-att.Timestamp) < 1 {
			return s.load(candidate)
		}
	}
	return HTMLAttachment{}, false
}

// mimeAppleArchive is the type given to the NSKeyedArchiver plists XCTest
// attaches for UI snapshots and synthesized events.
const mimeAppleArchive = "application/x-apple-plist"

// detectMIME identifies an attachment from its own bytes, which is the only
// dependable source: `xcresulttool export attachments` names most files after a
// bare UUID with no extension at all, and the attachment's label is often a
// constant like "kXCTAttachmentScreenRecording" rather than a file name.
func detectMIME(data []byte, fileName, label string) string {
	if mime := sniffMIME(data); mime != "" {
		return mime
	}
	return namedMIME(fileName, label)
}

// sniffMIME identifies a file from its magic bytes, returning "" when it
// recognises nothing.
func sniffMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	case bytes.HasPrefix(data, []byte("bplist00")):
		return mimeAppleArchive
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch string(data[8:12]) {
		case "heic", "heix", "heim", "heis", "hevc", "mif1", "msf1":
			return "image/heic"
		case "avif":
			return "image/avif"
		}
		// Simulator recordings are H.264 in a QuickTime-branded ISO container.
		// They are reported as MP4 regardless of brand: browsers refuse
		// video/quicktime by type even when they can decode the payload.
		return "video/mp4"
	}
	if looksLikeText(data) {
		return "text/plain"
	}
	return ""
}

// looksLikeText reports whether data reads as human-readable text: valid UTF-8
// with no NULs or stray control bytes in its opening block.
func looksLikeText(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	if !utf8.Valid(head) && !utf8.Valid(data) {
		return false
	}
	for _, b := range head {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			return false
		}
	}
	return true
}

// namedMIME infers a type from the exported file's extension, then from the
// attachment's label. It is the fallback for a file whose bytes said nothing.
func namedMIME(fileName, label string) string {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".heic":
		return "image/heic"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".txt", ".log":
		return "text/plain"
	}
	if strings.Contains(strings.ToLower(label), "recording") {
		return "video/mp4"
	}
	// Guessing "image/png" here is what put broken images in the report: an
	// unrecognised file is a download, not a screenshot.
	return "application/octet-stream"
}

// extraData is everything gathered outside the test tree itself.
type extraData struct {
	details     map[string]*TestDetails
	activities  map[string][]ActivityNode
	attachments *attachmentSet
	opts        HTMLOptions
}

// buildReport is the single-bundle case of combine.
func buildReport(summary *RunSummary, tests *Tests, extras extraData) *HTMLReport {
	bundle := &bundleData{
		summary:     summary,
		tests:       tests,
		details:     extras.details,
		activities:  extras.activities,
		attachments: extras.attachments,
	}
	if devices := summary.devices(tests); len(devices) == 1 {
		bundle.device = devices[0]
	}
	return combine([]*bundleData{bundle}, extras.opts)
}

// combine folds any number of result bundles into one report model. It is a
// pure transformation: no subprocesses run from here down, which is what makes
// it testable without Xcode.
func combine(bundles []*bundleData, opts HTMLOptions) *HTMLReport {
	report := &HTMLReport{
		Title:     opts.Title,
		Generator: opts.Generator,
	}

	var state buildState
	for _, bundle := range bundles {
		summary := bundle.summary
		if report.Title == "" {
			report.Title = summary.Title
		}
		if report.Environment == "" {
			report.Environment = summary.EnvironmentDescription
		}
		// The run spans from the earliest bundle to the latest. Taking the
		// first bundle's window instead is exactly the bug that makes a merged
		// bundle mis-report its own duration.
		report.StartTime = earliest(report.StartTime, unixTime(summary.StartTime))
		report.FinishTime = latest(report.FinishTime, unixTime(summary.FinishTime))
		report.Devices = addDevices(report.Devices, summary.devices(bundle.tests))

		extras := extraData{
			details:     bundle.details,
			activities:  bundle.activities,
			attachments: bundle.attachments,
			opts:        opts,
		}
		scope := buildScope{device: bundle.device}
		for i := range bundle.tests.TestNodes {
			report.Suites = mergeSuites(report.Suites, state.suites(&bundle.tests.TestNodes[i], scope, extras))
		}
	}

	// The caller's window, when there is one, beats anything derived from the
	// bundles: only whatever scheduled the run knows what it spent building and
	// enumerating before the first test started.
	if !opts.StartTime.IsZero() {
		report.StartTime = opts.StartTime
	}
	if !opts.FinishTime.IsZero() {
		report.FinishTime = opts.FinishTime
	}
	if !report.StartTime.IsZero() && !report.FinishTime.IsZero() {
		report.Seconds = report.FinishTime.Sub(report.StartTime).Seconds()
	}
	if report.Title == "" {
		report.Title = "Test Report"
	}

	sortSuites(report.Suites)
	report.Counts = countTests(report.Suites)
	report.Counts.Flaky = state.flaky
	report.Result = overallResult(report.Counts)
	for _, suite := range report.Suites {
		report.TestSeconds += suite.Seconds
	}
	return report
}

func earliest(a, b time.Time) time.Time {
	switch {
	case b.IsZero():
		return a
	case a.IsZero() || b.Before(a):
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// addDevices unions device lists, keying on whatever identifies a device: its
// UDID when the bundle records one, its name otherwise.
func addDevices(existing, incoming []DeviceInfo) []DeviceInfo {
	for _, device := range incoming {
		key := device.ID
		if key == "" {
			key = device.Name
		}
		var seen bool
		for _, have := range existing {
			haveKey := have.ID
			if haveKey == "" {
				haveKey = have.Name
			}
			if haveKey == key {
				seen = true
				break
			}
		}
		if !seen {
			existing = append(existing, device)
		}
	}
	return existing
}

// mergeSuites folds one bundle's suites into the report's, matching on name so
// that a class split across several batches appears once with all its tests.
func mergeSuites(existing, incoming []HTMLSuite) []HTMLSuite {
	for _, suite := range incoming {
		idx := -1
		for i := range existing {
			if existing[i].Name == suite.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			existing = append(existing, suite)
			continue
		}
		existing[idx].Classes = mergeClasses(existing[idx].Classes, suite.Classes)
		existing[idx].Seconds, existing[idx].Result = foldClasses(existing[idx].Classes)
	}
	return existing
}

func mergeClasses(existing, incoming []HTMLClass) []HTMLClass {
	for _, class := range incoming {
		idx := -1
		for i := range existing {
			if existing[i].Name == class.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			existing = append(existing, class)
			continue
		}
		existing[idx].Tests = append(existing[idx].Tests, class.Tests...)
		existing[idx].Seconds, existing[idx].Result = foldTests(existing[idx].Tests)
	}
	return existing
}

// sortSuites puts the report in a stable, predictable order. Failures are found
// through the filter box and the auto-expanded failing sections, so sorting by
// name beats sorting by result: the same test is in the same place every run.
func sortSuites(suites []HTMLSuite) {
	sort.Slice(suites, func(i, j int) bool { return suites[i].Name < suites[j].Name })
	for si := range suites {
		classes := suites[si].Classes
		sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
		for ci := range classes {
			tests := classes[ci].Tests
			sort.SliceStable(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
		}
	}
}

// countTests counts the report's headline figures from the rows themselves
// rather than from any bundle's summary.
//
// The counts have to agree with what the reader can see below them, and no
// single summary can supply that: one bundle's covers only its own batch, and a
// merged bundle's counts a retried test once while the tree shows both runs. A
// test is counted once, by its last result.
func countTests(suites []HTMLSuite) HTMLCounts {
	final := map[string]Result{}
	var order []string
	for _, suite := range suites {
		for _, class := range suite.Classes {
			for _, test := range class.Tests {
				if _, seen := final[test.Identifier]; !seen {
					order = append(order, test.Identifier)
				}
				final[test.Identifier] = test.Result
			}
		}
	}

	counts := HTMLCounts{Total: len(order)}
	for _, id := range order {
		switch result := final[id]; {
		case result == ResultPassed:
			counts.Passed++
		case result == ResultExpectedFailure:
			counts.ExpectedFailures++
		case result.Failed():
			counts.Failed++
		case result == ResultSkipped:
			counts.Skipped++
		}
	}
	return counts
}

func overallResult(counts HTMLCounts) Result {
	switch {
	case counts.Failed > 0:
		return ResultFailed
	case counts.Total == 0:
		return ResultUnknown
	case counts.Skipped == counts.Total:
		return ResultSkipped
	}
	return ResultPassed
}

// devices returns the devices the run used, preferring the summary's list and
// falling back to the test tree's.
func (s *RunSummary) devices(tests *Tests) []DeviceInfo {
	if len(s.DevicesAndConfigurations) > 0 {
		devices := make([]DeviceInfo, 0, len(s.DevicesAndConfigurations))
		for _, dc := range s.DevicesAndConfigurations {
			devices = append(devices, dc.Device)
		}
		return devices
	}
	return tests.Devices
}

type buildScope struct {
	target string
	// device is the simulator the enclosing bundle ran on. A per-batch bundle
	// names its device once, at the top, and not on the test nodes.
	device DeviceInfo
}

type buildState struct {
	flaky int
}

// suites maps the tree's node types onto the report's two visible levels.
// Everything above a bundle — test plan, configuration, device grouping — is a
// wrapper the report has no use for, so it is passed through.
func (st *buildState) suites(node *TestNode, scope buildScope, extras extraData) []HTMLSuite {
	switch node.NodeType {
	case nodeTypeUnitTestBundle, nodeTypeUITestBundle:
		scope.target = node.Name
		suite := HTMLSuite{Name: node.Name}
		for i := range node.Children {
			child := &node.Children[i]
			if child.NodeType == nodeTypeTestSuite {
				suite.Classes = append(suite.Classes, st.class(child, scope, nil, extras))
			}
		}
		suite.Seconds, suite.Result = foldClasses(suite.Classes)
		return []HTMLSuite{suite}

	case nodeTypeTestSuite:
		// A class with no bundle above it: show it as its own suite rather than
		// dropping it.
		class := st.class(node, scope, nil, extras)
		return []HTMLSuite{{
			Name:    node.Name,
			Seconds: class.Seconds,
			Result:  class.Result,
			Classes: []HTMLClass{class},
		}}

	default:
		var suites []HTMLSuite
		for i := range node.Children {
			suites = append(suites, st.suites(&node.Children[i], scope, extras)...)
		}
		return suites
	}
}

// class builds one test class. Nested suites — which parameterised tests
// produce — are flattened into the enclosing class rather than adding a level
// the reader has to expand.
func (st *buildState) class(node *TestNode, scope buildScope, path []string, extras extraData) HTMLClass {
	path = append(append([]string(nil), path...), node.Name)
	class := HTMLClass{Name: strings.Join(path, ".")}

	for i := range node.Children {
		child := &node.Children[i]
		switch child.NodeType {
		case nodeTypeTestCase:
			class.Tests = append(class.Tests, st.tests(child, scope, path, extras)...)
		case nodeTypeTestSuite:
			nested := st.class(child, scope, path, extras)
			class.Tests = append(class.Tests, nested.Tests...)
		}
	}

	class.Seconds, class.Result = foldTests(class.Tests)
	return class
}

// tests builds one entry per execution of a test case: one per device in a
// merged bundle, one in total otherwise.
func (st *buildState) tests(node *TestNode, scope buildScope, path []string, extras extraData) []HTMLTest {
	suite := strings.Join(path, "/")
	base := HTMLTest{
		Name:       node.Name,
		NodeID:     node.NodeIdentifier,
		Identifier: joinIdentifier(scope.target, node.NodeIdentifier, suite, node.Name),
		Seconds:    node.DurationInSeconds,
		Result:     normalizeResult(node.Result),
	}
	base.Attempts = extras.opts.Attempts[base.Identifier]
	base.Flaky = extras.opts.Flaky[base.Identifier]
	if base.Flaky {
		st.flaky++
	}

	// The details call is the only source of a source location, so prefer it
	// and fall back to the failure messages carried in the tree.
	if details, ok := extras.details[node.NodeIdentifier]; ok {
		base.FailureMessage, base.SourceLocation = sourceLocation(details)
	}
	base.Activities = convertActivities(extras.activities[node.NodeIdentifier], node.NodeIdentifier, extras.attachments)
	base.Attachments = unclaimedAttachments(node.NodeIdentifier, base.Activities, extras.attachments)

	var devices []*TestNode
	for i := range node.Children {
		if node.Children[i].NodeType == nodeTypeDevice {
			devices = append(devices, &node.Children[i])
		}
	}

	if len(devices) == 0 {
		test := base
		test.Device = scope.device.Name
		test.Failures = failuresFrom(node.Children)
		test.fillFailure()
		return []HTMLTest{test}
	}

	out := make([]HTMLTest, 0, len(devices))
	for _, device := range devices {
		test := base
		test.Device = device.Name
		test.Result = normalizeResult(device.Result)
		if device.DurationInSeconds > 0 {
			test.Seconds = device.DurationInSeconds
		}
		test.Failures = failuresFrom(device.Children)
		test.fillFailure()
		out = append(out, test)
	}
	return out
}

// fillFailure ensures a failed test shows something, even when the details call
// was skipped or returned nothing.
func (t *HTMLTest) fillFailure() {
	if t.FailureMessage != "" || len(t.Failures) == 0 {
		return
	}
	for _, f := range t.Failures {
		if f.Message != "" {
			t.FailureMessage = f.Message
			if t.SourceLocation == "" {
				t.SourceLocation = f.SourceCode
			}
			return
		}
	}
	t.SourceLocation = t.Failures[0].SourceCode
}

func convertActivities(nodes []ActivityNode, nodeID string, set *attachmentSet) []HTMLActivity {
	var out []HTMLActivity
	for _, n := range nodes {
		activity := HTMLActivity{
			Title:     n.Title,
			StartTime: unixTime(n.StartTime),
			Type:      n.ActivityType,
		}
		for _, att := range n.Attachments {
			if embedded, ok := set.forActivity(nodeID, att); ok {
				if att.Name != "" {
					embedded.Name = att.Name
				}
				activity.Attachments = append(activity.Attachments, embedded)
			}
		}
		activity.Children = convertActivities(n.ChildActivities, nodeID, set)
		out = append(out, activity)
	}
	return out
}

// unclaimedAttachments returns a test's exported files that no activity step
// accounted for. Without this, turning activity logs off would silently throw
// away every screenshot.
func unclaimedAttachments(nodeID string, activities []HTMLActivity, set *attachmentSet) []HTMLAttachment {
	if set == nil {
		return nil
	}
	claimed := map[string]bool{}
	var mark func(list []HTMLActivity)
	mark = func(list []HTMLActivity) {
		for _, a := range list {
			for _, att := range a.Attachments {
				claimed[att.file] = true
			}
			mark(a.Children)
		}
	}
	mark(activities)

	var out []HTMLAttachment
	for _, manifest := range set.byTest[nodeID] {
		embedded, ok := set.load(manifest)
		if !ok || claimed[embedded.file] {
			continue
		}
		claimed[embedded.file] = true
		out = append(out, embedded)
	}
	return out
}

// foldTests sums a class's running time and reduces its results to one. A class
// is failed if any of its tests failed, and skipped only if all of them were.
func foldTests(tests []HTMLTest) (float64, Result) {
	var seconds float64
	var failed, skipped, known int
	for _, t := range tests {
		seconds += t.Seconds
		switch {
		case t.Result.Failed():
			failed++
		case t.Result == ResultSkipped:
			skipped++
		}
		if t.Result != ResultUnknown {
			known++
		}
	}
	return seconds, foldResult(len(tests), known, failed, skipped)
}

func foldClasses(classes []HTMLClass) (float64, Result) {
	var seconds float64
	var failed, skipped, total int
	for _, c := range classes {
		seconds += c.Seconds
		total += len(c.Tests)
		switch {
		case c.Result.Failed():
			failed++
		case c.Result == ResultSkipped:
			skipped += len(c.Tests)
		}
	}
	if failed > 0 {
		return seconds, ResultFailed
	}
	if total > 0 && skipped == total {
		return seconds, ResultSkipped
	}
	if total == 0 {
		return seconds, ResultUnknown
	}
	return seconds, ResultPassed
}

func foldResult(total, known, failed, skipped int) Result {
	switch {
	case failed > 0:
		return ResultFailed
	case total == 0 || known == 0:
		return ResultUnknown
	case skipped == total:
		return ResultSkipped
	default:
		return ResultPassed
	}
}

func unixTime(ts float64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	sec := math.Floor(ts)
	return time.Unix(int64(sec), int64((ts-sec)*float64(time.Second)))
}

// ── Rendering ────────────────────────────────────────────────────────────────

//go:embed templates/report.html.tmpl
var htmlTemplate string

//go:embed templates/style.css
var htmlStyles string

var htmlFuncs = template.FuncMap{
	"resultClass":     resultClass,
	"resultLabel":     resultLabel,
	"formatSeconds":   formatDuration,
	"formatTime":      formatTime,
	"isImage":         func(mime string) bool { return strings.HasPrefix(mime, "image/") },
	"isVideo":         func(mime string) bool { return strings.HasPrefix(mime, "video/") },
	"isText":          func(mime string) bool { return strings.HasPrefix(mime, "text/") },
	"attachmentKind":  attachmentKind,
	"formatBytes":     formatBytes,
	"countActivities": countActivities,
	"anyFailed":       anyFailed,
	"formatPercent":   formatPercent,
	"coverageClass":   coverageClass,
	"coverageWidth":   coverageWidth,
}

var htmlTemplates = template.Must(template.New("report").Funcs(htmlFuncs).Parse(htmlTemplate))

// RenderHTML writes the report as a single self-contained HTML document.
func RenderHTML(w io.Writer, report *HTMLReport) error {
	data := struct {
		*HTMLReport
		CSS template.CSS
	}{report, template.CSS(htmlStyles)}

	if err := htmlTemplates.Execute(w, data); err != nil {
		return fmt.Errorf("render html report: %w", err)
	}
	return nil
}

// resultClass maps a result to the CSS class that colours it.
func resultClass(r Result) string {
	switch r {
	case ResultPassed:
		return "passed"
	case ResultFailed:
		return "failed"
	case ResultSkipped:
		return "skipped"
	case ResultExpectedFailure:
		return "expected-failure"
	default:
		return "unknown"
	}
}

func resultLabel(r Result) string {
	if r == ResultUnknown {
		return "No result"
	}
	return string(r)
}

func attachmentKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "attachment-img"
	case strings.HasPrefix(mime, "video/"):
		return "attachment-video"
	case strings.HasPrefix(mime, "text/"):
		return "attachment-text"
	default:
		return "attachment-file"
	}
}

// formatBytes renders a file size the way a reader thinks of one.
func formatBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

// countActivities counts a whole activity tree, so the collapsed summary line
// reports the real number of steps rather than just the top-level ones.
func countActivities(activities []HTMLActivity) int {
	n := 0
	for _, a := range activities {
		n += 1 + countActivities(a.Children)
	}
	return n
}

func anyFailed(tests []HTMLTest) bool {
	for _, t := range tests {
		if t.Result.Failed() {
			return true
		}
	}
	return false
}

// formatDuration renders a running time compactly: milliseconds below a second,
// then seconds, then minutes.
func formatDuration(seconds float64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds < 1:
		return fmt.Sprintf("%dms", int(math.Round(seconds*1000)))
	case seconds < 60:
		return fmt.Sprintf("%.1fs", seconds)
	}
	minutes := int(seconds) / 60
	rest := int(seconds) % 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, rest)
	}
	return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
}

func formatPercent(p float64) string { return fmt.Sprintf("%.2f%%", p) }

// coverageWidth is the inline width of a coverage bar. It is typed as CSS
// rather than left to the escaper, which cannot tell a safe percentage from an
// injected declaration and blanks the attribute rather than guess.
func coverageWidth(p float64) template.CSS {
	switch {
	case p < 0:
		p = 0
	case p > 100:
		p = 100
	}
	return template.CSS(fmt.Sprintf("width: %.2f%%", p))
}

// coverageClass buckets a percentage so the bar is readable at a glance. The
// thresholds are conventional rather than principled: no number here is a pass
// mark, and a UI suite's coverage is a map of what it exercised, not a score.
func coverageClass(p float64) string {
	switch {
	case p >= 75:
		return "high"
	case p >= 40:
		return "medium"
	default:
		return "low"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
