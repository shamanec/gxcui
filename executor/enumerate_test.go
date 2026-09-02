package executor

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// enumerationResponse stands in for xcodebuild: it writes the enumeration JSON
// to whatever path it was told to use.
func enumerationResponse(t *testing.T) exec.Response {
	t.Helper()
	body := fixture(t, "enumerate-flat.json")
	return exec.Response{Do: func(cmd exec.Command) error {
		for i, a := range cmd.Args {
			if a == "-test-enumeration-output-path" && i+1 < len(cmd.Args) {
				return os.WriteFile(cmd.Args[i+1], []byte(body), 0o644)
			}
		}
		t.Errorf("enumeration command has no output path: %s", cmd)
		return nil
	}}
}

// newTestExecutor builds an Executor backed by a scripted runner.
func newTestExecutor(cfg Config, responses ...exec.Response) (*Executor, *exec.Fake) {
	fake := exec.NewFake(responses...)
	return &Executor{cfg: &cfg, runner: fake}, fake
}

func xctestrunConfig() Config {
	cfg := DefaultConfig()
	cfg.Project.XCTestRun = "App.xctestrun"
	return cfg
}

func TestEnumerate(t *testing.T) {
	cfg := xctestrunConfig()
	e, fake := newTestExecutor(cfg,
		exec.Response{Stdout: fixture(t, "devices.json")},
		enumerationResponse(t),
	)

	got, err := e.Enumerate(context.Background(), EnumerateOptions{})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	// The first eligible booted simulator is used.
	if got.Device.Name != "xcpool-1" {
		t.Errorf("Device.Name = %q, want %q", got.Device.Name, "xcpool-1")
	}
	if got.Count() != 4 {
		t.Errorf("Count() = %d, want 4", got.Count())
	}
	if len(got.Plans) != 1 || got.Plans[0].Name != "Sample-Package" {
		t.Fatalf("Plans = %+v, want one plan named Sample-Package", got.Plans)
	}

	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want 2 (simctl, xcodebuild)", len(calls))
	}
	enumerate := calls[1].String()
	for _, want := range []string{
		"test-without-building",
		"-xctestrun App.xctestrun",
		"-destination \"platform=iOS Simulator,id=92F3C99D-476B-4BA5-B857-A7FAB6C60349\"",
		"-enumerate-tests",
		"-test-enumeration-style flat",
	} {
		if !strings.Contains(enumerate, want) {
			t.Errorf("enumeration command %q is missing %q", enumerate, want)
		}
	}
}

func TestEnumerateAppliesFilters(t *testing.T) {
	cfg := xctestrunConfig()
	cfg.Tests.Exclude = []string{"*/BetaTests/*"}

	e, _ := newTestExecutor(cfg,
		exec.Response{Stdout: fixture(t, "devices.json")},
		enumerationResponse(t),
	)

	got, err := e.Enumerate(context.Background(), EnumerateOptions{})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	want := []string{"SampleTests/AlphaTests/testOne()", "SampleTests/AlphaTests/testTwo()"}
	if strings.Join(got.Tests(), ",") != strings.Join(want, ",") {
		t.Errorf("Tests() = %v, want %v", got.Tests(), want)
	}
	if len(got.Plans[0].Filtered) != 2 {
		t.Errorf("Filtered = %v, want the two BetaTests", got.Plans[0].Filtered)
	}
}

func TestEnumerateOnNamedDevice(t *testing.T) {
	e, fake := newTestExecutor(xctestrunConfig(),
		exec.Response{Stdout: fixture(t, "devices.json")},
		enumerationResponse(t),
	)

	got, err := e.Enumerate(context.Background(), EnumerateOptions{Device: "xcpool-2"})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if got.Device.Name != "xcpool-2" {
		t.Errorf("Device.Name = %q, want %q", got.Device.Name, "xcpool-2")
	}
	if !strings.Contains(fake.Calls()[1].String(), got.Device.UDID) {
		t.Errorf("enumeration did not target %s", got.Device.UDID)
	}
}

func TestEnumerateRejectsUnbootedDevice(t *testing.T) {
	e, _ := newTestExecutor(xctestrunConfig(), exec.Response{Stdout: fixture(t, "devices.json")})

	_, err := e.Enumerate(context.Background(), EnumerateOptions{Device: "iPhone SE (3rd generation)"})
	if err == nil {
		t.Fatal("Enumerate() error = nil, want an error for a shut down simulator")
	}
	if !strings.Contains(err.Error(), "not booted") {
		t.Errorf("Enumerate() error = %q, want it to say the simulator is not booted", err)
	}
}

func TestEnumerateWithoutBootedSimulators(t *testing.T) {
	devices := `{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-5":[
		{"udid":"A","name":"idle","state":"Shutdown","isAvailable":true}]}}`
	e, _ := newTestExecutor(xctestrunConfig(), exec.Response{Stdout: devices})

	_, err := e.Enumerate(context.Background(), EnumerateOptions{})
	if err == nil {
		t.Fatal("Enumerate() error = nil, want an error when nothing is booted")
	}
	if !strings.Contains(err.Error(), "no booted simulators") {
		t.Errorf("Enumerate() error = %q, want it to explain that nothing is booted", err)
	}
}

func TestEnumerateWhenEveryDeviceIsFilteredOut(t *testing.T) {
	cfg := xctestrunConfig()
	cfg.Simulators.Exclude = []string{"xcpool-1", "xcpool-2"}

	e, _ := newTestExecutor(cfg, exec.Response{Stdout: fixture(t, "devices.json")})

	_, err := e.Enumerate(context.Background(), EnumerateOptions{})
	if err == nil {
		t.Fatal("Enumerate() error = nil, want an error when every simulator is excluded")
	}
	if !strings.Contains(err.Error(), "filtered out") {
		t.Errorf("Enumerate() error = %q, want it to blame the simulator filters", err)
	}
}

// With execution.xcodebuildOutput on, xcodebuild's own output reaches the
// caller untouched, while the enumerated tests still come back through the
// return value.
func TestEnumerateStreamsXcodebuildOutput(t *testing.T) {
	enumeration := enumerationResponse(t)
	enumeration.Stdout = "Test Suite 'All tests' started\n** TEST BUILD SUCCEEDED **\n"

	cfg := xctestrunConfig()
	cfg.Execution.XcodebuildOutput = true
	e, _ := newTestExecutor(cfg,
		exec.Response{Stdout: fixture(t, "devices.json")},
		enumeration,
	)

	var out bytes.Buffer
	got, err := e.Enumerate(context.Background(), EnumerateOptions{Output: &out})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if got.Count() == 0 {
		t.Error("Count() = 0, want the enumerated tests")
	}
	if out.String() != enumeration.Stdout {
		t.Errorf("stream = %q, want %q", out.String(), enumeration.Stdout)
	}
}

// The config half of the gate: a writer alone streams nothing.
func TestEnumerateDoesNotStreamUnlessConfigured(t *testing.T) {
	enumeration := enumerationResponse(t)
	enumeration.Stdout = "** TEST BUILD SUCCEEDED **\n"

	e, _ := newTestExecutor(xctestrunConfig(),
		exec.Response{Stdout: fixture(t, "devices.json")},
		enumeration,
	)

	var out bytes.Buffer
	if _, err := e.Enumerate(context.Background(), EnumerateOptions{Output: &out}); err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("streamed %q with execution.xcodebuildOutput off", out.String())
	}
}

func TestEnumerateDryRunRunsNothing(t *testing.T) {
	e, fake := newTestExecutor(xctestrunConfig(), exec.Response{Stdout: fixture(t, "devices.json")})

	got, err := e.Enumerate(context.Background(), EnumerateOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if got.Count() != 0 {
		t.Errorf("Count() = %d, want 0 for a dry run", got.Count())
	}
	if !strings.HasPrefix(got.Command, "xcodebuild test-without-building") {
		t.Errorf("Command = %q, want the xcodebuild invocation", got.Command)
	}
	// Only the simulator lookup should have run.
	if len(fake.Calls()) != 1 {
		t.Errorf("made %d calls, want 1 (simctl only)", len(fake.Calls()))
	}
}

func TestSelectDevices(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Simulators.Include = []string{"xcpool-2"}

	e, _ := newTestExecutor(cfg, exec.Response{Stdout: fixture(t, "devices.json")})

	sel, err := e.SelectDevices(context.Background())
	if err != nil {
		t.Fatalf("SelectDevices() error = %v", err)
	}
	if len(sel.Selected) != 1 || sel.Selected[0].Name != "xcpool-2" {
		t.Fatalf("Selected = %+v, want only xcpool-2", sel.Selected)
	}

	reasons := map[string]string{}
	for _, s := range sel.Skipped {
		reasons[s.Device.Name] = s.Reason
	}
	if got := reasons["xcpool-1"]; !strings.Contains(got, "not listed") {
		t.Errorf("xcpool-1 skipped because %q, want the include filter", got)
	}
	if got := reasons["iPhone SE (3rd generation)"]; !strings.Contains(got, "not booted") {
		t.Errorf("shut down device skipped because %q, want \"not booted\"", got)
	}
}
