package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shamanec/gxcui/internal/exec"
)

// htmlTests builds a small result tree: one bundle, two classes, one failure.
func htmlTests() *Tests {
	return &Tests{
		Devices: []DeviceInfo{{ID: "udid-1", Name: "iPhone 16", Platform: "iOS Simulator", OSVersion: "26.5", Architecture: "arm64"}},
		TestNodes: []TestNode{{
			NodeType: "Test Plan", Name: "Plan",
			Children: []TestNode{{
				NodeType: "UI test bundle", Name: "AppUITests",
				Children: []TestNode{
					{
						NodeType: "Test Suite", Name: "LoginTests",
						Children: []TestNode{
							{
								NodeType: "Test Case", Name: "testLogin()",
								NodeIdentifier: "LoginTests/testLogin()", Result: ResultPassed, DurationInSeconds: 12,
							},
							{
								NodeType: "Test Case", Name: "testLogout()",
								NodeIdentifier: "LoginTests/testLogout()", Result: ResultFailed, DurationInSeconds: 30,
								Children: []TestNode{{
									NodeType: "Failure Message",
									Name:     "XCTAssertTrue failed",
									Children: []TestNode{{NodeType: "Source Code Reference", Name: "LoginTests.swift:42"}},
								}},
							},
						},
					},
					{
						NodeType: "Test Suite", Name: "CheckoutTests",
						Children: []TestNode{{
							NodeType: "Test Case", Name: "testCheckout()",
							NodeIdentifier: "CheckoutTests/testCheckout()", Result: ResultSkipped,
						}},
					},
				},
			}},
		}},
	}
}

func htmlSummary() *RunSummary {
	return &RunSummary{
		Title:                  "Test - App",
		EnvironmentDescription: "App · Built with macOS 26.5.2",
		StartTime:              1786953946,
		FinishTime:             1786953996,
		Result:                 ResultFailed,
		TotalTestCount:         3,
		PassedTests:            1,
		FailedTests:            1,
		SkippedTests:           1,
		DevicesAndConfigurations: []DeviceSummary{
			{Device: DeviceInfo{Name: "iPhone 16", Platform: "iOS Simulator", OSVersion: "26.5", Architecture: "arm64"}},
		},
	}
}

func TestBuildReport(t *testing.T) {
	report := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{}})

	if report.Title != "Test - App" {
		t.Errorf("Title = %q, want the bundle's own title", report.Title)
	}
	if report.Seconds != 50 {
		t.Errorf("Seconds = %v, want 50 from the start and finish times", report.Seconds)
	}
	if report.Counts.Failed != 1 || report.Counts.Total != 3 {
		t.Errorf("Counts = %+v, want 3 total and 1 failed", report.Counts)
	}
	if len(report.Devices) != 1 || report.Devices[0].Name != "iPhone 16" {
		t.Errorf("Devices = %+v, want the summary's single device", report.Devices)
	}

	if len(report.Suites) != 1 {
		t.Fatalf("got %d suites, want the one bundle: %+v", len(report.Suites), report.Suites)
	}
	suite := report.Suites[0]
	if suite.Name != "AppUITests" {
		t.Errorf("suite name = %q, want the bundle name", suite.Name)
	}
	// The tree records no duration above a test case, so the suite and class
	// totals only exist if they are summed from the tests below them.
	if suite.Seconds != 42 {
		t.Errorf("suite duration = %v, want 42 summed from its tests", suite.Seconds)
	}
	if !suite.Result.Failed() {
		t.Errorf("suite result = %q, want failed because one test failed", suite.Result)
	}

	classes := map[string]HTMLClass{}
	for _, c := range suite.Classes {
		classes[c.Name] = c
	}
	if login := classes["LoginTests"]; login.Seconds != 42 || !login.Result.Failed() {
		t.Errorf("LoginTests = %v/%q, want 42s and failed", login.Seconds, login.Result)
	}
	if checkout := classes["CheckoutTests"]; checkout.Result != ResultSkipped {
		t.Errorf("CheckoutTests result = %q, want skipped when every test was", checkout.Result)
	}
}

// The full identifier is what everything else in gxcui keys on, and the result
// tree does not carry it: the target has to come from the enclosing bundle.
func TestBuildReportRebuildsIdentifiers(t *testing.T) {
	report := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{
		Attempts: map[string]int{"AppUITests/LoginTests/testLogout()": 2},
		Flaky:    map[string]bool{"AppUITests/LoginTests/testLogout()": true},
	}})

	var logout *HTMLTest
	for si := range report.Suites {
		for ci := range report.Suites[si].Classes {
			for ti, test := range report.Suites[si].Classes[ci].Tests {
				if test.Name == "testLogout()" {
					logout = &report.Suites[si].Classes[ci].Tests[ti]
				}
			}
		}
	}
	if logout == nil {
		t.Fatal("testLogout() is missing from the report")
	}
	if logout.Identifier != "AppUITests/LoginTests/testLogout()" {
		t.Errorf("Identifier = %q, want the target prefixed", logout.Identifier)
	}
	if logout.Attempts != 2 || !logout.Flaky {
		t.Errorf("Attempts/Flaky = %d/%v, want the run's retry information carried through", logout.Attempts, logout.Flaky)
	}
	if report.Counts.Flaky != 1 {
		t.Errorf("Counts.Flaky = %d, want 1", report.Counts.Flaky)
	}
	// With no details fetched, the failure still has to come from the tree.
	if logout.FailureMessage != "XCTAssertTrue failed" {
		t.Errorf("FailureMessage = %q, want the message carried in the tree", logout.FailureMessage)
	}
	if logout.SourceLocation != "LoginTests.swift:42" {
		t.Errorf("SourceLocation = %q, want the source reference below the failure", logout.SourceLocation)
	}
}

// A merged bundle inserts a Device level under each test case, and a retried
// test appears under more than one. Each execution is its own row.
func TestBuildReportSplitsPerDevice(t *testing.T) {
	tests := htmlTests()
	testCase := &tests.TestNodes[0].Children[0].Children[0].Children[1]
	testCase.Children = []TestNode{
		{NodeType: "Device", Name: "sim-1", Result: ResultFailed, DurationInSeconds: 30,
			Children: []TestNode{{NodeType: "Failure Message", Name: "first attempt failed"}}},
		{NodeType: "Device", Name: "sim-2", Result: ResultPassed, DurationInSeconds: 25},
	}

	report := buildReport(htmlSummary(), tests, extraData{opts: HTMLOptions{}})

	var rows []HTMLTest
	for _, c := range report.Suites[0].Classes {
		for _, test := range c.Tests {
			if test.Name == "testLogout()" {
				rows = append(rows, test)
			}
		}
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows for a test that ran twice, want 2", len(rows))
	}
	if rows[0].Device != "sim-1" || !rows[0].Result.Failed() {
		t.Errorf("first row = %+v, want the failing run on sim-1", rows[0])
	}
	if rows[1].Device != "sim-2" || rows[1].Result != ResultPassed {
		t.Errorf("second row = %+v, want the passing run on sim-2", rows[1])
	}
}

// A merged bundle's summary window is only as wide as its first input:
// xcresulttool merge does not union them. A run whose batches came in waves
// would otherwise report a fraction of its real duration, so the caller's
// window has to win.
func TestBuildReportPrefersTheCallersRunWindow(t *testing.T) {
	start := time.Date(2026, 8, 17, 16, 12, 41, 0, time.UTC)
	finish := start.Add(23*time.Minute + 22*time.Second)

	report := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{
		StartTime:  start,
		FinishTime: finish,
	}})

	if report.Seconds != 1402 {
		t.Errorf("Seconds = %v, want 1402 from the caller's window, not %v from the bundle's",
			report.Seconds, 50.0)
	}
	if !report.StartTime.Equal(start) {
		t.Errorf("StartTime = %v, want the caller's %v", report.StartTime, start)
	}

	// Without a window from the caller, the bundle's is all there is.
	fallback := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{}})
	if fallback.Seconds != 50 {
		t.Errorf("Seconds = %v, want the bundle's 50 when the caller gives no window", fallback.Seconds)
	}
}

// Total test time exceeding elapsed time is the whole point of running in
// parallel, so the report shows both — but only when there was parallelism to
// report.
func TestShowTestTime(t *testing.T) {
	report := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{}})

	if report.TestSeconds != 42 {
		t.Errorf("TestSeconds = %v, want 42 summed from the tests", report.TestSeconds)
	}
	// One device: elapsed and test time say the same thing.
	if report.ShowTestTime() {
		t.Error("test time is shown for a single-simulator run, where it adds nothing")
	}

	report.Devices = append(report.Devices, DeviceInfo{Name: "sim-2"})
	report.Seconds = 30
	if !report.ShowTestTime() {
		t.Error("test time is hidden even though 42s of tests finished in 30s across 2 simulators")
	}
}

// batchBundle builds what one per-batch result bundle looks like: a single
// device, its own time window, and only the tests that batch ran.
func batchBundle(device string, start, finish float64, class string, tests ...TestNode) *bundleData {
	info := DeviceInfo{ID: "udid-" + device, Name: device, Platform: "iOS Simulator", OSVersion: "26.5"}
	return &bundleData{
		path:   device + ".xcresult",
		device: info,
		summary: &RunSummary{
			Title:                    "Test - App",
			EnvironmentDescription:   "App · Built with macOS 26.5.2",
			StartTime:                start,
			FinishTime:               finish,
			DevicesAndConfigurations: []DeviceSummary{{Device: info}},
		},
		tests: &Tests{
			Devices: []DeviceInfo{info},
			TestNodes: []TestNode{{
				NodeType: "Test Plan", Name: "Plan",
				Children: []TestNode{{
					NodeType: "UI test bundle", Name: "AppUITests",
					Children: []TestNode{{NodeType: "Test Suite", Name: class, Children: tests}},
				}},
			}},
		},
	}
}

func testCaseNode(class, name string, result Result, seconds float64) TestNode {
	return TestNode{
		NodeType: "Test Case", Name: name,
		NodeIdentifier: class + "/" + name, Result: result, DurationInSeconds: seconds,
	}
}

// Reporting on the per-batch bundles directly has to reconstruct what a single
// run looked like: one suite, the classes merged, every simulator listed, and a
// window spanning the earliest batch to the latest.
func TestCombineBundles(t *testing.T) {
	report := combine([]*bundleData{
		batchBundle("sim-1", 1000, 1600, "LoginTests",
			testCaseNode("LoginTests", "testLogin()", ResultPassed, 12)),
		batchBundle("sim-2", 1100, 2000, "LoginTests",
			testCaseNode("LoginTests", "testLogout()", ResultFailed, 30)),
		batchBundle("sim-2", 2000, 2400, "CheckoutTests",
			testCaseNode("CheckoutTests", "testCheckout()", ResultPassed, 20)),
	}, HTMLOptions{})

	// The window spans the earliest start to the latest finish. Taking the
	// first bundle's — which is what xcresulttool merge does — would report 600.
	if report.Seconds != 1400 {
		t.Errorf("Seconds = %v, want 1400 spanning every bundle", report.Seconds)
	}
	if report.TestSeconds != 62 {
		t.Errorf("TestSeconds = %v, want 62 summed across bundles", report.TestSeconds)
	}

	if len(report.Devices) != 2 {
		t.Errorf("Devices = %+v, want the two simulators, deduplicated", report.Devices)
	}
	if !report.ShowDevices() {
		t.Error("device badges are hidden even though the run used two simulators")
	}

	// One bundle per batch, but one report: the suite appears once and the two
	// batches of LoginTests are folded into a single class.
	if len(report.Suites) != 1 {
		t.Fatalf("got %d suites, want the one bundle they all share: %+v", len(report.Suites), report.Suites)
	}
	suite := report.Suites[0]
	if len(suite.Classes) != 2 {
		t.Fatalf("got %d classes, want CheckoutTests and LoginTests: %+v", len(suite.Classes), suite.Classes)
	}
	if suite.Classes[0].Name != "CheckoutTests" || suite.Classes[1].Name != "LoginTests" {
		t.Errorf("classes = %q, %q; want them sorted by name", suite.Classes[0].Name, suite.Classes[1].Name)
	}
	login := suite.Classes[1]
	if len(login.Tests) != 2 {
		t.Fatalf("LoginTests has %d tests, want both batches' worth", len(login.Tests))
	}
	if !login.Result.Failed() {
		t.Errorf("LoginTests result = %q, want failed: one of its batches failed", login.Result)
	}

	// Each row says which simulator ran it — the thing merging throws away.
	devices := map[string]string{}
	for _, test := range login.Tests {
		devices[test.Name] = test.Device
	}
	if devices["testLogin()"] != "sim-1" || devices["testLogout()"] != "sim-2" {
		t.Errorf("row devices = %+v, want each test attributed to its own batch's simulator", devices)
	}

	if report.Counts.Total != 3 || report.Counts.Passed != 2 || report.Counts.Failed != 1 {
		t.Errorf("Counts = %+v, want 3 total, 2 passed, 1 failed", report.Counts)
	}
	if !report.Result.Failed() {
		t.Errorf("Result = %q, want failed", report.Result)
	}
}

// A retried test ran twice and shows two rows, but it is one test. Summing the
// bundles' own totals would count it twice and disagree with the tree below.
func TestCombineCountsRetriedTestsOnce(t *testing.T) {
	report := combine([]*bundleData{
		batchBundle("sim-1", 1000, 1600, "LoginTests",
			testCaseNode("LoginTests", "testFlaky()", ResultFailed, 30)),
		batchBundle("sim-2", 1600, 1700, "LoginTests",
			testCaseNode("LoginTests", "testFlaky()", ResultPassed, 28)),
	}, HTMLOptions{})

	if report.Counts.Total != 1 {
		t.Errorf("Total = %d, want 1: the same test ran twice", report.Counts.Total)
	}
	// The later run is the one that decides, so a test that was rescued by a
	// retry does not leave the report claiming a failure.
	if report.Counts.Passed != 1 || report.Counts.Failed != 0 {
		t.Errorf("Counts = %+v, want the last attempt to decide", report.Counts)
	}
	// Both attempts stay visible.
	if got := len(report.Suites[0].Classes[0].Tests); got != 2 {
		t.Errorf("got %d rows, want both attempts shown", got)
	}
}

// One simulator means the device is the same on every row and already in the
// header, so the badge is suppressed.
func TestCombineHidesDeviceBadgeForOneSimulator(t *testing.T) {
	report := combine([]*bundleData{
		batchBundle("sim-1", 1000, 1600, "LoginTests",
			testCaseNode("LoginTests", "testLogin()", ResultPassed, 12)),
	}, HTMLOptions{})

	if report.ShowDevices() {
		t.Error("device badges are shown for a one-simulator run, where they say nothing")
	}
	if !strings.Contains(renderReport(t, report), "sim-1") {
		t.Error("the simulator is missing from the report entirely")
	}
}

func renderReport(t *testing.T, report *HTMLReport) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderHTML(&buf, report); err != nil {
		t.Fatalf("RenderHTML() error = %v", err)
	}
	return buf.String()
}

func TestRenderHTML(t *testing.T) {
	report := buildReport(htmlSummary(), htmlTests(), extraData{opts: HTMLOptions{
		Flaky:     map[string]bool{"AppUITests/LoginTests/testLogout()": true},
		Attempts:  map[string]int{"AppUITests/LoginTests/testLogout()": 2},
		Generator: "gxcui test",
	}})
	out := renderReport(t, report)

	for _, want := range []string{
		"<!DOCTYPE html>",
		"XCTAssertTrue failed",
		"LoginTests.swift:42",
		"testLogin()",
		`class="badge flaky"`,
		"iPhone 16",
		"gxcui test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	// The stylesheet is inlined, so the file works with nothing beside it.
	if !strings.Contains(out, "--surface:") {
		t.Error("the stylesheet was not inlined")
	}
}

// A failure message is attacker-controlled in the sense that it is whatever the
// test printed, so it must never be able to close a tag.
func TestRenderHTMLEscapesTestOutput(t *testing.T) {
	tests := htmlTests()
	failure := &tests.TestNodes[0].Children[0].Children[0].Children[1].Children[0]
	failure.Name = `<script>alert("xss")</script>`

	out := renderReport(t, buildReport(htmlSummary(), tests, extraData{opts: HTMLOptions{}}))

	if strings.Contains(out, "<script>alert") {
		t.Error("a failure message was rendered as markup")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("the escaped failure message is missing from the report")
	}
}

func TestWriteHTMLReadsTheBundle(t *testing.T) {
	summary, err := json.Marshal(htmlSummary())
	if err != nil {
		t.Fatal(err)
	}
	tests, err := json.Marshal(htmlTests())
	if err != nil {
		t.Fatal(err)
	}

	fake := exec.NewFake(
		exec.Response{Stdout: string(summary)},
		exec.Response{Stdout: string(tests)},
		// The failing test's details, fetched for its source location.
		exec.Response{Stdout: `{"testRuns":[{"nodeType":"Test Case Run","name":"assertion failed","result":"Failed",
			"children":[{"nodeType":"Source Code Reference","name":"LoginTests.swift:99"}]}]}`},
	)
	// Anything after that — the attachment export — finds nothing.
	fake.Default = &exec.Response{}

	path := t.TempDir() + "/report.html"
	err = NewWithRunner(fake).WriteHTML(context.Background(), "merged.xcresult", path, HTMLOptions{
		Activities: DetailNone,
	})
	if err != nil {
		t.Fatalf("WriteHTML() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	out := string(written)
	if !strings.Contains(out, "LoginTests.swift:99") {
		t.Error("the source location from test-details is missing, so details were not used")
	}

	// Only the failing test may cost a details call; fetching them for passing
	// tests is the difference between a fast report and a slow one.
	var detailCalls int
	for _, call := range fake.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "test-details") {
			detailCalls++
		}
	}
	if detailCalls != 1 {
		t.Errorf("made %d test-details calls, want 1 for the single failure", detailCalls)
	}
}

func TestDetailWants(t *testing.T) {
	tests := []struct {
		detail Detail
		result Result
		want   bool
	}{
		{DetailNone, ResultFailed, false},
		{DetailFailed, ResultFailed, true},
		{DetailFailed, ResultPassed, false},
		{DetailAll, ResultPassed, true},
		{DetailAll, ResultSkipped, true},
	}
	for _, tt := range tests {
		if got := tt.detail.wants(tt.result); got != tt.want {
			t.Errorf("Detail(%q).wants(%q) = %v, want %v", tt.detail, tt.result, got, tt.want)
		}
	}
	if Detail("some").Valid() {
		t.Error("an unknown detail level was accepted")
	}
}

func TestDetectMIME(t *testing.T) {
	// xcresulttool exports most attachments under a bare UUID with no
	// extension, so the bytes have to carry the answer.
	tests := []struct {
		name              string
		data              []byte
		file, label, want string
	}{
		{"png bytes", []byte("\x89PNG\r\n\x1a\nrest"), "6D12F145", "UI Snapshot", "image/png"},
		{"jpeg bytes", []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0}, "abc", "", "image/jpeg"},
		{"quicktime-branded recording", []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00"), "abc", "kXCTAttachmentScreenRecording", "video/mp4"},
		{"mp4 recording", []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00"), "abc.mp4", "", "video/mp4"},
		{"heic screenshot", []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), "abc", "", "image/heic"},
		// An XCUIElement archive is XCTest's own bookkeeping. Calling it a PNG
		// is what put broken images in the report.
		{"element archive", []byte("bplist00\xd4\x00\x01\x00\x02"), "abc", "UI Snapshot 2026-03-12", mimeAppleArchive},
		{"ui hierarchy dump", []byte("Attributes: Application, pid: 1\n  Window\n"), "abc.txt", "App UI hierarchy", "text/plain"},
		// Nothing recognisable: the extension, then the label, then a download.
		{"unknown with extension", []byte{0x01, 0x02, 0x03, 0x04}, "abc.mov", "", "video/quicktime"},
		{"unknown with label", []byte{0x01, 0x02, 0x03, 0x04}, "abc", "Screen Recording 2026-03-12", "video/mp4"},
		{"unknown entirely", []byte{0x01, 0x02, 0x03, 0x04}, "abc", "whatever", "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectMIME(tt.data, tt.file, tt.label); got != tt.want {
				t.Errorf("detectMIME(%q, %q) = %q, want %q", tt.file, tt.label, got, tt.want)
			}
		})
	}
}

// A screen recording is not one of the attachments Xcode ties to a failure, so
// asking xcresulttool for the failures' attachments drops it. The report has to
// export everything and pick the failing tests' files itself.
func TestCollectAttachmentsKeepsAFailedTestsRecording(t *testing.T) {
	dir := t.TempDir()
	recording := "6D12F145-B038-4EFD-BFC5-0E8D6F218EC0.mp4"
	writeFile(t, filepath.Join(dir, recording), []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00"))
	writeFile(t, filepath.Join(dir, "AD4DD9F3.png"), []byte("\x89PNG\r\n\x1a\npassing"))
	writeFile(t, filepath.Join(dir, "manifest.json"), []byte(`[
		{"testIdentifier":"LoginTests/testLogout()","attachments":[
			{"exportedFileName":"`+recording+`","suggestedHumanReadableName":"Screen Recording.mp4","isAssociatedWithFailure":false}]},
		{"testIdentifier":"LoginTests/testLogin()","attachments":[
			{"exportedFileName":"AD4DD9F3.png","suggestedHumanReadableName":"Screenshot","isAssociatedWithFailure":false}]}
	]`))

	// The export itself must not be narrowed to the failures' attachments.
	fake := exec.NewFake()
	fake.Default = &exec.Response{}
	if _, cleanup, err := NewWithRunner(fake).collectAttachments(context.Background(), "batch.xcresult",
		HTMLOptions{Attachments: DetailFailed}, map[string]bool{"LoginTests/testLogout()": true}); err != nil {
		t.Fatalf("collectAttachments() error = %v", err)
	} else {
		defer cleanup()
	}
	for _, call := range fake.Calls() {
		if strings.Contains(strings.Join(call.Args, " "), "--only-failures") {
			t.Error("the export asked xcresulttool for only the failures' attachments, which excludes screen recordings")
		}
	}

	// What the export would have written is the manifest above; only the failed
	// test's files may survive the indexing.
	set, err := indexManifest(dir, 0, map[string]bool{"LoginTests/testLogout()": true})
	if err != nil {
		t.Fatalf("indexManifest() error = %v", err)
	}
	if got := len(set.byTest["LoginTests/testLogout()"]); got != 1 {
		t.Fatalf("kept %d attachments for the failed test, want its recording", got)
	}
	if got := len(set.byTest["LoginTests/testLogin()"]); got != 0 {
		t.Errorf("kept %d attachments for a passing test, want none", got)
	}

	att, ok := set.load(set.byTest["LoginTests/testLogout()"][0])
	if !ok {
		t.Fatal("the recording was not loaded")
	}
	if att.MIMEType != "video/mp4" {
		t.Errorf("recording MIME = %q, want video/mp4", att.MIMEType)
	}
	if att.Data == "" {
		t.Error("the recording was not embedded")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, ""},
		{512, "512 B"},
		{2048, "2 KB"},
		{4561014, "4.3 MB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.bytes); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{0, ""},
		{0.25, "250ms"},
		{12.34, "12.3s"},
		{95, "1m 35s"},
		{3900, "1h 5m"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.seconds); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}
