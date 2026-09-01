// Package reporter turns Xcode result bundles into reports.
//
// The input is one or more .xcresult bundles, read through
// `xcrun xcresulttool get test-results tests`, whose JSON schema the tool itself
// publishes (`xcresulttool get test-results tests --schema`). The types below
// mirror that schema exactly.
package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Node types reported by xcresulttool.
const (
	nodeTypeUnitTestBundle = "Unit test bundle"
	nodeTypeUITestBundle   = "UI test bundle"
	nodeTypeTestSuite      = "Test Suite"
	nodeTypeTestCase       = "Test Case"
	nodeTypeDevice         = "Device"
	nodeTypeFailureMessage = "Failure Message"
	nodeTypeSourceCodeRef  = "Source Code Reference"
)

// Result is the outcome of a test as xcresulttool reports it.
type Result string

// Results xcresulttool can report. Anything unrecognised becomes ResultUnknown.
const (
	ResultPassed          Result = "Passed"
	ResultFailed          Result = "Failed"
	ResultSkipped         Result = "Skipped"
	ResultExpectedFailure Result = "Expected Failure"
	ResultUnknown         Result = "unknown"
)

// Passed reports whether the result counts as success. An expected failure is a
// test that failed on purpose, so it passes.
func (r Result) Passed() bool { return r == ResultPassed || r == ResultExpectedFailure }

// Failed reports whether the result counts as a failure.
func (r Result) Failed() bool { return r == ResultFailed }

// Tests is the top level of `xcresulttool get test-results tests` output.
type Tests struct {
	TestPlanConfigurations []Configuration `json:"testPlanConfigurations"`
	Devices                []DeviceInfo    `json:"devices"`
	TestNodes              []TestNode      `json:"testNodes"`
}

// Configuration is a test plan configuration.
type Configuration struct {
	ID   string `json:"configurationId"`
	Name string `json:"configurationName"`
}

// DeviceInfo describes a device tests ran on.
type DeviceInfo struct {
	ID           string `json:"deviceId"`
	Name         string `json:"deviceName"`
	Architecture string `json:"architecture"`
	ModelName    string `json:"modelName"`
	Platform     string `json:"platform"`
	OSVersion    string `json:"osVersion"`
	OSBuild      string `json:"osBuildNumber"`
}

// TestNode is one node of the result tree. The same type covers every level:
// test plan, bundle, suite, test case, device, failure message and so on.
type TestNode struct {
	NodeIdentifier    string     `json:"nodeIdentifier"`
	NodeIdentifierURL string     `json:"nodeIdentifierURL"`
	NodeType          string     `json:"nodeType"`
	Name              string     `json:"name"`
	Details           string     `json:"details"`
	Duration          string     `json:"duration"`
	DurationInSeconds float64    `json:"durationInSeconds"`
	Result            Result     `json:"result"`
	Tags              []string   `json:"tags"`
	Children          []TestNode `json:"children"`
}

// Failure is one reported failure of a test.
type Failure struct {
	Message    string `json:"message"`
	SourceCode string `json:"sourceCode,omitempty"`
}

// TestCase is one test, flattened out of the result tree.
type TestCase struct {
	// Identifier is the full "Target/Class/method()" form, the same one
	// -only-testing takes. xcresulttool reports test cases without the target,
	// so it is reassembled from the enclosing bundle node.
	Identifier string `json:"identifier"`
	// Target is the test bundle, e.g. "MyAppUITests".
	Target string `json:"target"`
	// Suite is the class path within the target.
	Suite string `json:"suite"`
	// Name is the test method, e.g. "testLogin()".
	Name string `json:"name"`

	Result   Result  `json:"result"`
	Duration float64 `json:"durationSeconds"`

	// Device names the device the test ran on. A bundle produced by
	// xcresulttool merge reports one entry per device; a single-device bundle
	// leaves this filled in from the bundle's device list.
	Device string `json:"device,omitempty"`
	// DeviceID is the device's UDID when the bundle records one.
	DeviceID string `json:"deviceId,omitempty"`

	Failures []Failure `json:"failures,omitempty"`

	// UITest reports whether the test came from a UI test bundle.
	UITest bool `json:"uiTest,omitempty"`
}

// ParseTests flattens the JSON of `xcresulttool get test-results tests`.
//
// It handles both shapes the tool produces. In a single-device bundle a test
// case's children are its failure messages; after xcresulttool merge, each test
// case instead gains one Device child per device it ran on, and the failure
// messages hang off those. A test that ran on several devices yields one
// TestCase per device.
func ParseTests(data []byte) ([]TestCase, error) {
	var doc Tests
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse test results: %w", err)
	}

	// Fall back to the bundle's device list when test cases carry no Device
	// nodes of their own, which is the single-device case.
	var fallback DeviceInfo
	if len(doc.Devices) == 1 {
		fallback = doc.Devices[0]
	}

	var cases []TestCase
	for i := range doc.TestNodes {
		walk(&doc.TestNodes[i], walkState{fallback: fallback}, &cases)
	}
	return cases, nil
}

type walkState struct {
	target   string
	suite    []string
	uiTest   bool
	fallback DeviceInfo
}

func walk(node *TestNode, state walkState, out *[]TestCase) {
	switch node.NodeType {
	case nodeTypeUnitTestBundle, nodeTypeUITestBundle:
		state.target = node.Name
		state.uiTest = node.NodeType == nodeTypeUITestBundle
		state.suite = nil

	case nodeTypeTestSuite:
		state.suite = append(append([]string(nil), state.suite...), node.Name)

	case nodeTypeTestCase:
		*out = append(*out, testCasesFrom(node, state)...)
		return
	}

	for i := range node.Children {
		walk(&node.Children[i], state, out)
	}
}

// testCasesFrom turns one Test Case node into one result per device.
func testCasesFrom(node *TestNode, state walkState) []TestCase {
	base := TestCase{
		Target:   state.target,
		Suite:    strings.Join(state.suite, "/"),
		Name:     node.Name,
		Result:   normalizeResult(node.Result),
		Duration: node.DurationInSeconds,
		UITest:   state.uiTest,
	}
	base.Identifier = joinIdentifier(state.target, node.NodeIdentifier, base.Suite, node.Name)

	var devices []TestNode
	for _, child := range node.Children {
		if child.NodeType == nodeTypeDevice {
			devices = append(devices, child)
		}
	}

	if len(devices) == 0 {
		// Single-device bundle: failures hang directly off the test case.
		result := base
		result.Device = state.fallback.Name
		result.DeviceID = state.fallback.ID
		result.Failures = failuresFrom(node.Children)
		return []TestCase{result}
	}

	cases := make([]TestCase, 0, len(devices))
	for _, device := range devices {
		result := base
		result.Device = device.Name
		result.DeviceID = device.NodeIdentifier
		result.Result = normalizeResult(device.Result)
		if device.DurationInSeconds > 0 {
			result.Duration = device.DurationInSeconds
		}
		result.Failures = failuresFrom(device.Children)
		cases = append(cases, result)
	}
	return cases
}

func failuresFrom(children []TestNode) []Failure {
	var failures []Failure
	for _, child := range children {
		switch child.NodeType {
		case nodeTypeFailureMessage:
			failures = append(failures, Failure{
				Message:    child.Name,
				SourceCode: sourceCodeRef(child.Children),
			})
		case nodeTypeSourceCodeRef:
			// A source reference with no failure message above it still marks
			// where something went wrong.
			failures = append(failures, Failure{SourceCode: child.Name})
		}
	}
	return failures
}

func sourceCodeRef(children []TestNode) string {
	for _, child := range children {
		if child.NodeType == nodeTypeSourceCodeRef {
			return child.Name
		}
	}
	return ""
}

// joinIdentifier reassembles the full test identifier.
//
// A Test Case node's nodeIdentifier omits the target: it reads
// "BetaTests/testFails()", not "SampleTests/BetaTests/testFails()". The target
// comes from the enclosing bundle node.
func joinIdentifier(target, nodeIdentifier, suite, name string) string {
	tail := nodeIdentifier
	if tail == "" {
		tail = strings.TrimPrefix(suite+"/"+name, "/")
	}
	if target == "" {
		return tail
	}
	if strings.HasPrefix(tail, target+"/") {
		return tail
	}
	return target + "/" + tail
}

func normalizeResult(r Result) Result {
	switch r {
	case ResultPassed, ResultFailed, ResultSkipped, ResultExpectedFailure:
		return r
	default:
		return ResultUnknown
	}
}

// Reporter reads result bundles.
type Reporter struct {
	runner exec.Runner
}

// New returns a Reporter that shells out to xcresulttool.
func New() *Reporter { return &Reporter{runner: exec.OS{}} }

// NewWithRunner returns a Reporter that runs commands through r.
//
// It exists so the rest of gxcui can share one command runner, and so tests can
// substitute a scripted one. The runner type is internal to gxcui; outside
// callers want New.
func NewWithRunner(r exec.Runner) *Reporter { return &Reporter{runner: r} }

// ReadBundle returns the test results recorded in one .xcresult bundle.
func (r *Reporter) ReadBundle(ctx context.Context, path string) ([]TestCase, error) {
	res, err := r.runner.Run(ctx, exec.Command{
		Name: "xcrun",
		Args: []string{"xcresulttool", "get", "test-results", "tests", "--path", path, "--compact"},
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read %s: xcresulttool exited %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return ParseTests([]byte(res.Stdout))
}
