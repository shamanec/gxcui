package reporter

import (
	"context"
	"os"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func byIdentifier(cases []TestCase) map[string]TestCase {
	m := make(map[string]TestCase, len(cases))
	for _, c := range cases {
		m[c.Identifier] = c
	}
	return m
}

// A single-device bundle hangs failure messages directly off the test case and
// records the device only in the top-level device list.
func TestParseTestsSingleDevice(t *testing.T) {
	cases, err := ParseTests(fixture(t, "tests-single.json"))
	if err != nil {
		t.Fatalf("ParseTests() error = %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want 2: %+v", len(cases), cases)
	}

	got := byIdentifier(cases)

	// The identifier must be reassembled with the target, which xcresulttool
	// omits from a test case's nodeIdentifier.
	failed, ok := got["SampleTests/BetaTests/testFails()"]
	if !ok {
		t.Fatalf("missing SampleTests/BetaTests/testFails(), got %v", keys(got))
	}
	if failed.Result != ResultFailed {
		t.Errorf("Result = %q, want %q", failed.Result, ResultFailed)
	}
	if failed.Target != "SampleTests" || failed.Suite != "BetaTests" || failed.Name != "testFails()" {
		t.Errorf("split = (%q, %q, %q), want (SampleTests, BetaTests, testFails())", failed.Target, failed.Suite, failed.Name)
	}
	if failed.Device != "xcpool-2" {
		t.Errorf("Device = %q, want xcpool-2 from the bundle device list", failed.Device)
	}
	if len(failed.Failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failed.Failures))
	}
	if want := `AlphaTests.swift:11: XCTAssertEqual failed: ("2") is not equal to ("3")`; failed.Failures[0].Message != want {
		t.Errorf("failure message = %q, want %q", failed.Failures[0].Message, want)
	}
	// durationInSeconds is the only safe duration source: the human-readable
	// "duration" field is locale-formatted ("0,24s" here).
	if failed.Duration < 0.2 || failed.Duration > 0.3 {
		t.Errorf("Duration = %v, want roughly 0.24", failed.Duration)
	}

	passed := got["SampleTests/BetaTests/testThree()"]
	if passed.Result != ResultPassed {
		t.Errorf("Result = %q, want %q", passed.Result, ResultPassed)
	}
	if len(passed.Failures) != 0 {
		t.Errorf("passing test has failures: %+v", passed.Failures)
	}
}

// A merged bundle adds a Device level under each test case, and moves the
// failure messages down onto it.
func TestParseTestsMerged(t *testing.T) {
	cases, err := ParseTests(fixture(t, "tests-merged.json"))
	if err != nil {
		t.Fatalf("ParseTests() error = %v", err)
	}
	if len(cases) != 4 {
		t.Fatalf("got %d cases, want 4: %+v", len(cases), cases)
	}

	got := byIdentifier(cases)
	failed, ok := got["SampleTests/BetaTests/testFails()"]
	if !ok {
		t.Fatalf("missing the failing test, got %v", keys(got))
	}
	if failed.Result != ResultFailed {
		t.Errorf("Result = %q, want %q", failed.Result, ResultFailed)
	}
	if failed.Device != "iPhone SE (3rd generation)" {
		t.Errorf("Device = %q, want the device node's name", failed.Device)
	}
	if failed.DeviceID != "8442C46F-83D8-4A3B-8F34-47A4CE4C34D9" {
		t.Errorf("DeviceID = %q, want the UDID from the device node", failed.DeviceID)
	}
	if len(failed.Failures) != 1 {
		t.Fatalf("got %d failures, want 1 from under the device node", len(failed.Failures))
	}

	// The two batches ran on different simulators; both must be represented.
	devices := map[string]bool{}
	for _, c := range cases {
		devices[c.DeviceID] = true
	}
	if len(devices) != 2 {
		t.Errorf("got %d distinct devices, want 2: %v", len(devices), devices)
	}
}

func TestParseTestsRejectsGarbage(t *testing.T) {
	if _, err := ParseTests([]byte("not json")); err == nil {
		t.Fatal("ParseTests() error = nil, want a parse error")
	}
}

func TestParseTestsUnknownResult(t *testing.T) {
	data := []byte(`{"devices":[],"testPlanConfigurations":[],"testNodes":[
		{"nodeType":"Test Plan","name":"P","children":[
			{"nodeType":"UI test bundle","name":"UITests","children":[
				{"nodeType":"Test Suite","name":"LoginTests","children":[
					{"nodeType":"Test Case","name":"testA()","nodeIdentifier":"LoginTests/testA()","result":"Sideways"}]}]}]}]}`)

	cases, err := ParseTests(data)
	if err != nil {
		t.Fatalf("ParseTests() error = %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	if cases[0].Result != ResultUnknown {
		t.Errorf("Result = %q, want %q for an unrecognised value", cases[0].Result, ResultUnknown)
	}
	if !cases[0].UITest {
		t.Error("UITest = false, want true for a UI test bundle")
	}
	if cases[0].Identifier != "UITests/LoginTests/testA()" {
		t.Errorf("Identifier = %q, want UITests/LoginTests/testA()", cases[0].Identifier)
	}
}

func TestReadBundle(t *testing.T) {
	fake := exec.NewFake(exec.Response{Stdout: string(fixture(t, "tests-single.json"))})
	r := &Reporter{runner: fake}

	cases, err := r.ReadBundle(context.Background(), "out/batch-01.xcresult")
	if err != nil {
		t.Fatalf("ReadBundle() error = %v", err)
	}
	if len(cases) != 2 {
		t.Errorf("got %d cases, want 2", len(cases))
	}

	want := "xcrun xcresulttool get test-results tests --path out/batch-01.xcresult --compact"
	if got := fake.Calls()[0].String(); got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestReadBundleNonZeroExit(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 1, Stderr: "bundle not found"})
	r := &Reporter{runner: fake}

	if _, err := r.ReadBundle(context.Background(), "missing.xcresult"); err == nil {
		t.Fatal("ReadBundle() error = nil, want an error")
	}
}

func TestResultHelpers(t *testing.T) {
	if !ResultExpectedFailure.Passed() {
		t.Error("an expected failure should count as passed")
	}
	if ResultSkipped.Passed() || ResultSkipped.Failed() {
		t.Error("skipped should be neither passed nor failed")
	}
	if !ResultFailed.Failed() {
		t.Error("failed should count as failed")
	}
}

func keys(m map[string]TestCase) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
