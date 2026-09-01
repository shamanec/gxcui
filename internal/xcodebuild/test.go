package xcodebuild

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/shamanec/gxcui/internal/exec"
)

// Exit codes xcodebuild uses that gxcui reads meaning into. Anything else is
// treated as an infrastructure failure rather than a test failure.
const (
	// ExitOK means every test passed.
	ExitOK = 0
	// ExitTestFailure means the run completed with failing tests. It is the
	// only non-zero code that says anything about the tests themselves.
	ExitTestFailure = 65
)

// MaxTestsPerInvocation caps how many -only-testing arguments go into a single
// command. Each one is an argv entry, and a large enough suite would otherwise
// run into the operating system's argument length limit.
const MaxTestsPerInvocation = 1000

// TestOptions describes one batch of tests running on one destination.
type TestOptions struct {
	Project Project
	// Destination is the simulator to run on.
	Destination string
	// OnlyTesting lists the test identifiers to run. Empty runs everything the
	// test plan enables.
	OnlyTesting []string
	// ResultBundlePath is where xcodebuild writes the .xcresult bundle. It must
	// not already exist.
	ResultBundlePath string
	// TestTimeout, when positive, caps how long any single test may run.
	TestTimeout int
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Stdout and Stderr, when set, receive the process output as it arrives.
	Stdout io.Writer
	Stderr io.Writer
}

// TestArgs renders the argument vector for one batch.
func TestArgs(opts TestOptions) ([]string, error) {
	if err := opts.Project.Validate(); err != nil {
		return nil, err
	}
	if opts.Destination == "" {
		return nil, fmt.Errorf("destination is required to run tests")
	}
	if opts.ResultBundlePath == "" {
		return nil, fmt.Errorf("result bundle path is required")
	}
	if len(opts.OnlyTesting) > MaxTestsPerInvocation {
		return nil, fmt.Errorf("%d tests in one invocation exceeds the limit of %d: split the batch",
			len(opts.OnlyTesting), MaxTestsPerInvocation)
	}

	args := []string{opts.Project.Action()}
	args = append(args, opts.Project.args()...)
	args = append(args,
		"-destination", opts.Destination,
		"-resultBundlePath", opts.ResultBundlePath,
		// gxcui is the parallelism. Letting xcodebuild clone simulators as well
		// would have two schedulers fighting over the same machine.
		"-parallel-testing-enabled", "NO",
	)
	if opts.TestTimeout > 0 {
		args = append(args,
			"-test-timeouts-enabled", "YES",
			"-maximum-test-execution-time-allowance", strconv.Itoa(opts.TestTimeout),
		)
	}
	for _, test := range opts.OnlyTesting {
		args = append(args, "-only-testing:"+test)
	}
	args = append(args, opts.ExtraArgs...)
	return args, nil
}

// TestRun is the outcome of one batch invocation.
type TestRun struct {
	// ExitCode is xcodebuild's exit status.
	ExitCode int
	// Command is the invocation, for logs and manifests.
	Command string
}

// TestsFailed reports whether xcodebuild ran the tests and some of them failed,
// as opposed to failing before or during the run for another reason.
func (r TestRun) TestsFailed() bool { return r.ExitCode == ExitTestFailure }

// OK reports whether every test passed.
func (r TestRun) OK() bool { return r.ExitCode == ExitOK }

// RunTests executes one batch.
//
// A non-zero exit is returned in TestRun rather than as an error: only exit 65
// carries information about the tests, and every other code needs the result
// bundle to interpret. An error means the process could not be run at all.
func RunTests(ctx context.Context, r exec.Runner, opts TestOptions) (*TestRun, error) {
	args, err := TestArgs(opts)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command{Name: Binary, Args: args, Stdout: opts.Stdout, Stderr: opts.Stderr}
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &TestRun{ExitCode: res.ExitCode, Command: cmd.String()}, nil
}

// TestCommand renders the invocation without running it, for --dry-run.
func TestCommand(opts TestOptions) (string, error) {
	args, err := TestArgs(opts)
	if err != nil {
		return "", err
	}
	return exec.Command{Name: Binary, Args: args}.String(), nil
}
