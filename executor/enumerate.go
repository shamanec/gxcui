package executor

import (
	"context"
	"fmt"

	"github.com/shamanec/gxcui/internal/xcodebuild"
)

// EnumerateOptions tunes a single discovery run.
type EnumerateOptions struct {
	// Device selects the simulator to enumerate on, by UDID or name. Empty
	// means the first eligible simulator.
	Device string
	// DryRun builds the command without running it. The returned Enumeration
	// carries only the Command field.
	DryRun bool
}

// EnumeratedPlan is one test plan's worth of discovered tests.
type EnumeratedPlan struct {
	// Name is the test plan the tests belong to.
	Name string `json:"testPlan"`
	// Tests are the tests that would run, after tests.include/exclude.
	Tests []string `json:"tests"`
	// Filtered are enabled tests dropped by tests.include/exclude.
	Filtered []string `json:"filtered,omitempty"`
	// Disabled are tests the test plan itself turns off. They never run and are
	// not subject to filtering.
	Disabled []string `json:"disabled,omitempty"`
}

// Enumeration is the result of discovering the tests in a project.
type Enumeration struct {
	// Device is the simulator enumeration ran on.
	Device Device `json:"device"`
	// Command is the xcodebuild invocation used, for --dry-run and logs.
	Command string `json:"command"`
	// Plans holds one entry per test plan reported by xcodebuild.
	Plans []EnumeratedPlan `json:"plans"`
}

// Tests returns every test that would run, across all plans.
func (e *Enumeration) Tests() []string {
	var all []string
	for _, p := range e.Plans {
		all = append(all, p.Tests...)
	}
	return all
}

// Count returns the number of tests that would run.
func (e *Enumeration) Count() int {
	var n int
	for _, p := range e.Plans {
		n += len(p.Tests)
	}
	return n
}

// Enumerate discovers the tests in the configured project.
//
// It runs xcodebuild once, in flat enumeration style, on a single simulator.
// Enumeration is not free — xcodebuild installs and launches the test host to
// ask it what it contains — so callers that need the list repeatedly should hold
// on to the result.
func (e *Executor) Enumerate(ctx context.Context, opts EnumerateOptions) (*Enumeration, error) {
	device, err := e.pickDevice(ctx, opts.Device)
	if err != nil {
		return nil, err
	}

	if opts.DryRun {
		command, err := xcodebuild.EnumerateCommand(xcodebuild.EnumerateOptions{
			Project:     e.cfg.xcodebuildProject(),
			Destination: xcodebuild.SimulatorDestination(device.UDID),
			Style:       xcodebuild.StyleFlat,
		})
		if err != nil {
			return nil, err
		}
		return &Enumeration{Device: device, Command: command}, nil
	}
	return e.enumerateProject(ctx, e.cfg.xcodebuildProject(), device)
}

// enumerateProject discovers the tests in an already-resolved project.
//
// The run path calls this with the built .xctestrun rather than the source
// project, so that discovery and execution are guaranteed to be looking at the
// same build.
func (e *Executor) enumerateProject(ctx context.Context, project xcodebuild.Project, device Device) (*Enumeration, error) {
	filter, err := NewFilter(e.cfg.Tests.Include, e.cfg.Tests.Exclude)
	if err != nil {
		return nil, err
	}

	xcOpts := xcodebuild.EnumerateOptions{
		Project:     project,
		Destination: xcodebuild.SimulatorDestination(device.UDID),
		Style:       xcodebuild.StyleFlat,
	}

	command, err := xcodebuild.EnumerateCommand(xcOpts)
	if err != nil {
		return nil, err
	}
	result := &Enumeration{Device: device, Command: command}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.Tests.Enumerate.Timeout.Duration())
	defer cancel()

	enumeration, err := xcodebuild.Enumerate(ctx, e.runner, xcOpts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("enumerate tests: timed out after %s: raise tests.enumerate.timeout", e.cfg.Tests.Enumerate.Timeout)
		}
		return nil, err
	}

	for _, plan := range enumeration.Values {
		enabled := identifiers(plan.EnabledTests)
		kept, dropped := filter.Apply(enabled)
		result.Plans = append(result.Plans, EnumeratedPlan{
			Name:     plan.TestPlan,
			Tests:    kept,
			Filtered: dropped,
			Disabled: identifiers(plan.DisabledTests),
		})
	}

	if len(result.Plans) == 0 {
		return nil, fmt.Errorf("enumerate tests: xcodebuild reported no test plans for this project")
	}
	return result, nil
}

func identifiers(tests []xcodebuild.TestIdentifier) []string {
	if len(tests) == 0 {
		return nil
	}
	out := make([]string, 0, len(tests))
	for _, t := range tests {
		out = append(out, t.Identifier)
	}
	return out
}
