package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Node types that only appear in `test-details` output.
const (
	nodeTypeTestCaseRun = "Test Case Run"
)

// RunSummary is the output of `xcresulttool get test-results summary`.
//
// It is the only place the run's own metadata lives — its title, wall-clock
// window and the environment it was built in. The test tree carries none of
// that.
type RunSummary struct {
	Title                  string `json:"title"`
	EnvironmentDescription string `json:"environmentDescription"`
	// StartTime and FinishTime are UNIX timestamps in seconds.
	StartTime  float64 `json:"startTime"`
	FinishTime float64 `json:"finishTime"`
	Result     Result  `json:"result"`

	TotalTestCount   int `json:"totalTestCount"`
	PassedTests      int `json:"passedTests"`
	FailedTests      int `json:"failedTests"`
	SkippedTests     int `json:"skippedTests"`
	ExpectedFailures int `json:"expectedFailures"`

	TopInsights  []Insight     `json:"topInsights"`
	Statistics   []Statistic   `json:"statistics"`
	TestFailures []TestFailure `json:"testFailures"`

	DevicesAndConfigurations []DeviceSummary `json:"devicesAndConfigurations"`
}

// Insight is an observation Xcode surfaces about the run, such as a test that
// fails consistently or one that is unusually slow.
type Insight struct {
	Impact   string `json:"impact"`
	Category string `json:"category"`
	Text     string `json:"text"`
}

// Statistic is a headline figure Xcode reports, e.g. "4 tests ran on 2 devices".
type Statistic struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
}

// TestFailure is the summary-level record of one failing test.
type TestFailure struct {
	TestName string `json:"testName"`
	// TestIdentifierString is the "Class/method()" form. The numeric
	// testIdentifier alongside it is an internal index, not an identifier
	// anything else understands.
	TestIdentifierString string `json:"testIdentifierString"`
	TargetName           string `json:"targetName"`
	FailureText          string `json:"failureText"`
}

// DeviceSummary is the per-device breakdown of a run.
type DeviceSummary struct {
	Device                DeviceInfo    `json:"device"`
	TestPlanConfiguration Configuration `json:"testPlanConfiguration"`
	PassedTests           int           `json:"passedTests"`
	FailedTests           int           `json:"failedTests"`
	SkippedTests          int           `json:"skippedTests"`
	ExpectedFailures      int           `json:"expectedFailures"`
}

// TestDetails is the output of `xcresulttool get test-results test-details`.
//
// The test tree already names a failure; this adds the source location, which
// is what turns "XCTAssertEqual failed" into something you can jump to.
type TestDetails struct {
	TestIdentifier  string     `json:"testIdentifier"`
	TestName        string     `json:"testName"`
	TestDescription string     `json:"testDescription"`
	TestResult      Result     `json:"testResult"`
	TestRuns        []TestNode `json:"testRuns"`
}

// Activities is the output of `xcresulttool get test-results activities`: the
// step-by-step log of what a test did, and where its screenshots were taken.
type Activities struct {
	TestIdentifier string `json:"testIdentifier"`
	TestName       string `json:"testName"`
	// TestRuns holds one entry per execution. A retried test has several, most
	// recent last.
	TestRuns []TestRunActivities `json:"testRuns"`
}

// TestRunActivities is the activity log of a single execution of a test.
type TestRunActivities struct {
	Device                DeviceInfo     `json:"device"`
	TestPlanConfiguration Configuration  `json:"testPlanConfiguration"`
	Activities            []ActivityNode `json:"activities"`
}

// ActivityNode is one step in a test's activity log. Steps nest, forming the
// tree Xcode shows in its test report.
type ActivityNode struct {
	Title           string               `json:"title"`
	StartTime       float64              `json:"startTime"`
	ActivityType    string               `json:"activityType"`
	Attachments     []ActivityAttachment `json:"attachments"`
	ChildActivities []ActivityNode       `json:"childActivities"`
}

// ActivityAttachment references a file captured during a step. The file itself
// lives inside the bundle until ExportAttachments writes it out.
type ActivityAttachment struct {
	Name      string  `json:"name"`
	PayloadID string  `json:"payloadId"`
	UUID      string  `json:"uuid"`
	Timestamp float64 `json:"timestamp"`
	Lifetime  string  `json:"lifetime"`
}

// AttachmentManifest is the manifest.json written by
// `xcresulttool export attachments`. It is the only reliable way to map an
// attachment to the file that was written for it: the exported name is chosen
// by xcresulttool, not derived from anything in the activity log.
type AttachmentManifest []TestAttachments

// TestAttachments lists the files exported for one test.
type TestAttachments struct {
	TestIdentifier string               `json:"testIdentifier"`
	Attachments    []ManifestAttachment `json:"attachments"`
}

// ManifestAttachment describes one exported file.
type ManifestAttachment struct {
	ExportedFileName           string  `json:"exportedFileName"`
	SuggestedHumanReadableName string  `json:"suggestedHumanReadableName"`
	IsAssociatedWithFailure    bool    `json:"isAssociatedWithFailure"`
	Timestamp                  float64 `json:"timestamp"`
	ConfigurationName          string  `json:"configurationName"`
	DeviceName                 string  `json:"deviceName"`
	DeviceID                   string  `json:"deviceId"`
}

// resultTool runs an xcresulttool subcommand and returns its stdout.
func (r *Reporter) resultTool(ctx context.Context, args ...string) ([]byte, error) {
	res, err := r.runner.Run(ctx, exec.Command{
		Name: "xcrun",
		Args: append([]string{"xcresulttool"}, args...),
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("xcresulttool %s exited %d: %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// ReadSummary returns the run-level summary of a result bundle.
func (r *Reporter) ReadSummary(ctx context.Context, path string) (*RunSummary, error) {
	out, err := r.resultTool(ctx, "get", "test-results", "summary", "--path", path, "--compact")
	if err != nil {
		return nil, fmt.Errorf("read summary of %s: %w", path, err)
	}
	var summary RunSummary
	if err := json.Unmarshal(out, &summary); err != nil {
		return nil, fmt.Errorf("parse summary of %s: %w", path, err)
	}
	return &summary, nil
}

// ReadRawTests returns the unparsed test tree, for callers that need the tree
// itself rather than the flattened cases ParseTests produces.
func (r *Reporter) ReadRawTests(ctx context.Context, path string) (*Tests, error) {
	out, err := r.resultTool(ctx, "get", "test-results", "tests", "--path", path, "--compact")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var tests Tests
	if err := json.Unmarshal(out, &tests); err != nil {
		return nil, fmt.Errorf("parse test results of %s: %w", path, err)
	}
	return &tests, nil
}

// ReadTestDetails returns the detail record of one test. testID is a Test Case
// node's nodeIdentifier, which omits the target: "LoginTests/testFoo()".
func (r *Reporter) ReadTestDetails(ctx context.Context, path, testID string) (*TestDetails, error) {
	out, err := r.resultTool(ctx, "get", "test-results", "test-details",
		"--path", path, "--test-id", testID, "--compact")
	if err != nil {
		return nil, fmt.Errorf("read details of %s: %w", testID, err)
	}
	var details TestDetails
	if err := json.Unmarshal(out, &details); err != nil {
		return nil, fmt.Errorf("parse details of %s: %w", testID, err)
	}
	return &details, nil
}

// ReadActivities returns the activity log of one test.
func (r *Reporter) ReadActivities(ctx context.Context, path, testID string) (*Activities, error) {
	out, err := r.resultTool(ctx, "get", "test-results", "activities",
		"--path", path, "--test-id", testID, "--compact")
	if err != nil {
		return nil, fmt.Errorf("read activities of %s: %w", testID, err)
	}
	var activities Activities
	if err := json.Unmarshal(out, &activities); err != nil {
		return nil, fmt.Errorf("parse activities of %s: %w", testID, err)
	}
	return &activities, nil
}

// ExportAttachments writes every attachment in the bundle to outputDir, along
// with a manifest.json describing them. With onlyFailures set, only attachments
// belonging to failing tests are written — which for a large UI test run is the
// difference between megabytes and gigabytes.
func (r *Reporter) ExportAttachments(ctx context.Context, path, outputDir string, onlyFailures bool) error {
	args := []string{"export", "attachments", "--path", path, "--output-path", outputDir}
	if onlyFailures {
		args = append(args, "--only-failures")
	}
	if _, err := r.resultTool(ctx, args...); err != nil {
		return fmt.Errorf("export attachments of %s: %w", path, err)
	}
	return nil
}

// sourceLocation walks a test-details tree for the file:line of the first
// failure it finds.
//
// The location is a "Source Code Reference" child of either a "Failure Message"
// or a "Test Case Run" node, depending on how the failure was raised.
func sourceLocation(details *TestDetails) (message, location string) {
	for i := range details.TestRuns {
		if msg, loc, ok := walkForFailure(&details.TestRuns[i]); ok {
			return msg, loc
		}
	}
	return "", ""
}

func walkForFailure(node *TestNode) (message, location string, found bool) {
	if node.Result.Failed() && node.Name != "" &&
		(node.NodeType == nodeTypeFailureMessage || node.NodeType == nodeTypeTestCaseRun) {
		return node.Name, sourceCodeRef(node.Children), true
	}
	for i := range node.Children {
		if msg, loc, ok := walkForFailure(&node.Children[i]); ok {
			return msg, loc, true
		}
	}
	return "", "", false
}
