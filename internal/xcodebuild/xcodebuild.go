// Package xcodebuild builds xcodebuild invocations and interprets their output.
//
// The package owns two things the rest of gxcui should not have to know about:
// the exact argument vectors xcodebuild expects, and the shape of the JSON it
// produces. Argument construction is deliberately separated from execution so
// that it can be unit-tested and printed by --dry-run.
package xcodebuild

import (
	"context"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Binary is the executable gxcui drives.
const Binary = "xcodebuild"

// Actions used by gxcui.
const (
	ActionTest                = "test"
	ActionTestWithoutBuilding = "test-without-building"
	ActionBuildForTesting     = "build-for-testing"
)

// Project identifies what to test and how it was configured. Exactly one of
// XCTestRun, TestProducts, Project or Workspace must be set.
type Project struct {
	// XCTestRun is a path to an .xctestrun file produced by build-for-testing.
	// It is the preferred input: it needs no build and can be reused by any
	// number of concurrent xcodebuild processes.
	XCTestRun string
	// TestProducts is a path to an .xctestproducts archive.
	TestProducts string
	// Project is a path to an .xcodeproj.
	Project string
	// Workspace is a path to an .xcworkspace.
	Workspace string

	// Scheme is required with Project or Workspace and ignored otherwise.
	Scheme string
	// TestPlan is the name of a test plan associated with the scheme, without
	// the .xctestplan extension. Optional.
	TestPlan string
	// Configuration is the build configuration, e.g. "Debug". Optional.
	Configuration string
	// DerivedDataPath isolates build products and logs. Optional but strongly
	// recommended when several xcodebuild processes run at once.
	DerivedDataPath string
}

// prebuilt reports whether the project can be tested without a build step.
func (p Project) prebuilt() bool {
	return p.XCTestRun != "" || p.TestProducts != ""
}

// Validate checks that the input combination is one xcodebuild accepts.
func (p Project) Validate() error {
	var set []string
	for _, in := range []struct {
		name  string
		value string
	}{
		{"xctestrun", p.XCTestRun},
		{"testProducts", p.TestProducts},
		{"project", p.Project},
		{"workspace", p.Workspace},
	} {
		if in.value != "" {
			set = append(set, in.name)
		}
	}

	switch len(set) {
	case 0:
		return fmt.Errorf("no test input: set one of xctestrun, testProducts, project or workspace")
	case 1:
	default:
		return fmt.Errorf("conflicting test inputs %s: set exactly one", strings.Join(set, ", "))
	}

	if p.prebuilt() {
		// -xctestrun and -testProductsPath carry the scheme's settings already,
		// and xcodebuild rejects them alongside -project/-workspace/-scheme.
		if p.Scheme != "" {
			return fmt.Errorf("scheme cannot be combined with %s", set[0])
		}
		return nil
	}
	if p.Scheme == "" {
		return fmt.Errorf("scheme is required with %s", set[0])
	}
	return nil
}

// args renders the project as xcodebuild flags, without an action.
func (p Project) args() []string {
	var a []string
	switch {
	case p.XCTestRun != "":
		a = append(a, "-xctestrun", p.XCTestRun)
	case p.TestProducts != "":
		a = append(a, "-testProductsPath", p.TestProducts)
	case p.Workspace != "":
		a = append(a, "-workspace", p.Workspace, "-scheme", p.Scheme)
	case p.Project != "":
		a = append(a, "-project", p.Project, "-scheme", p.Scheme)
	}
	if p.TestPlan != "" && !p.prebuilt() {
		// A test plan is selected through the scheme; an xctestrun file was
		// already generated for one specific plan.
		a = append(a, "-testPlan", p.TestPlan)
	}
	if p.Configuration != "" {
		a = append(a, "-configuration", p.Configuration)
	}
	if p.DerivedDataPath != "" {
		a = append(a, "-derivedDataPath", p.DerivedDataPath)
	}
	return a
}

// Action returns the test action appropriate for the input: prebuilt inputs skip
// compilation, everything else has to build first.
func (p Project) Action() string {
	if p.prebuilt() {
		return ActionTestWithoutBuilding
	}
	return ActionTest
}

// Version returns the first line of `xcodebuild -version`, e.g. "Xcode 26.6".
func Version(ctx context.Context, r exec.Runner) (string, error) {
	res, err := r.Run(ctx, exec.Command{Name: Binary, Args: []string{"-version"}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("xcodebuild -version: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(res.Stdout), "\n")
	return strings.TrimSpace(line), nil
}

// SimulatorDestination renders a destination specifier for a simulator UDID.
func SimulatorDestination(udid string) string {
	return "platform=iOS Simulator,id=" + udid
}

// failureError describes a non-zero xcodebuild exit.
func failureError(what string, res *exec.Result) error {
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = lastLines(res.Stdout, 15)
	}
	return fmt.Errorf("%s: xcodebuild exited %d\n%s", what, res.ExitCode, detail)
}

// lastLines returns the final n non-empty lines of s, which is usually where
// xcodebuild puts the reason it gave up.
func lastLines(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
