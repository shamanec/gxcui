package xcodebuild

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

const testUDID = "92F3C99D-476B-4BA5-B857-A7FAB6C60349"

func TestEnumerateArgs(t *testing.T) {
	tests := []struct {
		name string
		opts EnumerateOptions
		want []string
	}{
		{
			name: "prebuilt xctestrun needs no build",
			opts: EnumerateOptions{
				Project:     Project{XCTestRun: "/tmp/App.xctestrun"},
				Destination: SimulatorDestination(testUDID),
			},
			want: []string{
				"test-without-building",
				"-xctestrun", "/tmp/App.xctestrun",
				"-destination", "platform=iOS Simulator,id=" + testUDID,
				"-enumerate-tests",
				"-test-enumeration-style", "flat",
				"-test-enumeration-format", "json",
				"-test-enumeration-output-path", "/tmp/out.json",
			},
		},
		{
			name: "workspace and scheme build first",
			opts: EnumerateOptions{
				Project: Project{
					Workspace:       "App.xcworkspace",
					Scheme:          "AppUITests",
					TestPlan:        "Smoke",
					Configuration:   "Debug",
					DerivedDataPath: ".gxcui/dd",
				},
				Destination: SimulatorDestination(testUDID),
				Style:       StyleHierarchical,
			},
			want: []string{
				"test",
				"-workspace", "App.xcworkspace",
				"-scheme", "AppUITests",
				"-testPlan", "Smoke",
				"-configuration", "Debug",
				"-derivedDataPath", ".gxcui/dd",
				"-destination", "platform=iOS Simulator,id=" + testUDID,
				"-enumerate-tests",
				"-test-enumeration-style", "hierarchical",
				"-test-enumeration-format", "json",
				"-test-enumeration-output-path", "/tmp/out.json",
			},
		},
		{
			name: "extra args are appended verbatim",
			opts: EnumerateOptions{
				Project:     Project{Project: "App.xcodeproj", Scheme: "App"},
				Destination: "platform=iOS Simulator,name=iPhone 16",
				ExtraArgs:   []string{"-quiet"},
			},
			want: []string{
				"test",
				"-project", "App.xcodeproj",
				"-scheme", "App",
				"-destination", "platform=iOS Simulator,name=iPhone 16",
				"-enumerate-tests",
				"-test-enumeration-style", "flat",
				"-test-enumeration-format", "json",
				"-test-enumeration-output-path", "/tmp/out.json",
				"-quiet",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EnumerateArgs(tt.opts, "/tmp/out.json")
			if err != nil {
				t.Fatalf("EnumerateArgs() error = %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("EnumerateArgs()\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}

func TestEnumerateArgsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		opts EnumerateOptions
		want string
	}{
		{
			name: "no input",
			opts: EnumerateOptions{Destination: "platform=iOS Simulator,id=x"},
			want: "no test input",
		},
		{
			name: "two inputs",
			opts: EnumerateOptions{
				Project:     Project{XCTestRun: "a.xctestrun", Workspace: "App.xcworkspace"},
				Destination: "platform=iOS Simulator,id=x",
			},
			want: "conflicting test inputs",
		},
		{
			name: "scheme missing",
			opts: EnumerateOptions{
				Project:     Project{Workspace: "App.xcworkspace"},
				Destination: "platform=iOS Simulator,id=x",
			},
			want: "scheme is required",
		},
		{
			name: "scheme with prebuilt input",
			opts: EnumerateOptions{
				Project:     Project{XCTestRun: "a.xctestrun", Scheme: "App"},
				Destination: "platform=iOS Simulator,id=x",
			},
			want: "scheme cannot be combined",
		},
		{
			name: "no destination",
			opts: EnumerateOptions{Project: Project{XCTestRun: "a.xctestrun"}},
			want: "destination is required",
		},
		{
			name: "unknown style",
			opts: EnumerateOptions{
				Project:     Project{XCTestRun: "a.xctestrun"},
				Destination: "platform=iOS Simulator,id=x",
				Style:       "sideways",
			},
			want: "unknown enumeration style",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EnumerateArgs(tt.opts, "/tmp/out.json")
			if err == nil {
				t.Fatalf("EnumerateArgs() error = nil, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("EnumerateArgs() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseFlat(t *testing.T) {
	data, err := os.ReadFile("testdata/enumerate-flat.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	got, err := ParseFlat(data)
	if err != nil {
		t.Fatalf("ParseFlat() error = %v", err)
	}
	if len(got.Values) != 1 {
		t.Fatalf("ParseFlat() returned %d plans, want 1", len(got.Values))
	}

	plan := got.Values[0]
	if plan.TestPlan != "Sample-Package" {
		t.Errorf("TestPlan = %q, want %q", plan.TestPlan, "Sample-Package")
	}
	want := []string{
		"SampleTests/AlphaTests/testOne()",
		"SampleTests/AlphaTests/testTwo()",
		"SampleTests/BetaTests/testFails()",
		"SampleTests/BetaTests/testThree()",
	}
	if len(plan.EnabledTests) != len(want) {
		t.Fatalf("got %d enabled tests, want %d", len(plan.EnabledTests), len(want))
	}
	for i, test := range plan.EnabledTests {
		if test.Identifier != want[i] {
			t.Errorf("enabledTests[%d] = %q, want %q", i, test.Identifier, want[i])
		}
	}
	if len(plan.DisabledTests) != 0 {
		t.Errorf("got %d disabled tests, want 0", len(plan.DisabledTests))
	}
}

func TestParseFlatReportsErrors(t *testing.T) {
	data := []byte(`{"errors":[{"message":"the destination is not available"}],"values":[]}`)

	_, err := ParseFlat(data)
	if err == nil {
		t.Fatal("ParseFlat() error = nil, want the reported error")
	}
	if !strings.Contains(err.Error(), "the destination is not available") {
		t.Errorf("ParseFlat() error = %q, want it to carry the xcodebuild message", err)
	}
}

func TestEnumerateReadsOutputFile(t *testing.T) {
	fixture, err := os.ReadFile("testdata/enumerate-flat.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Stand in for xcodebuild: write the enumeration to the path it was told to.
	fake := exec.NewFake(exec.Response{Do: func(cmd exec.Command) error {
		return os.WriteFile(outputPathFrom(cmd.Args), fixture, 0o644)
	}})

	got, err := Enumerate(context.Background(), fake, EnumerateOptions{
		Project:     Project{XCTestRun: "App.xctestrun"},
		Destination: SimulatorDestination(testUDID),
	})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	if len(got.Values) != 1 || len(got.Values[0].EnabledTests) != 4 {
		t.Fatalf("Enumerate() returned %+v, want 1 plan with 4 tests", got.Values)
	}
}

func TestEnumerateFailsWhenNoOutputWritten(t *testing.T) {
	fake := exec.NewFake(exec.Response{Stdout: "** TEST EXECUTE SUCCEEDED **"})

	_, err := Enumerate(context.Background(), fake, EnumerateOptions{
		Project:     Project{XCTestRun: "App.xctestrun"},
		Destination: SimulatorDestination(testUDID),
	})
	if err == nil {
		t.Fatal("Enumerate() error = nil, want an error when xcodebuild wrote nothing")
	}
	if !strings.Contains(err.Error(), "wrote no output") {
		t.Errorf("Enumerate() error = %q, want it to explain the missing output", err)
	}
}

func TestEnumerateFailsOnNonZeroExit(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 70, Stderr: "xcodebuild: error: unknown scheme"})

	_, err := Enumerate(context.Background(), fake, EnumerateOptions{
		Project:     Project{XCTestRun: "App.xctestrun"},
		Destination: SimulatorDestination(testUDID),
	})
	if err == nil {
		t.Fatal("Enumerate() error = nil, want an error for exit 70")
	}
	if !strings.Contains(err.Error(), "unknown scheme") {
		t.Errorf("Enumerate() error = %q, want it to carry xcodebuild's message", err)
	}
}

// outputPathFrom extracts the value of -test-enumeration-output-path.
func outputPathFrom(args []string) string {
	for i, a := range args {
		if a == "-test-enumeration-output-path" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
