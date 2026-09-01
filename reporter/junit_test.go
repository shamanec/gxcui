package reporter

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func junitCases() []TestCase {
	return []TestCase{
		{
			Identifier: "App/LoginTests/testA()", Target: "App", Suite: "LoginTests", Name: "testA()",
			Result: ResultPassed, Duration: 1.25, Device: "sim-1",
		},
		{
			Identifier: "App/LoginTests/testB()", Target: "App", Suite: "LoginTests", Name: "testB()",
			Result: ResultFailed, Duration: 2.5, Device: "sim-1",
			Failures: []Failure{{Message: "LoginTests.swift:42: XCTAssertTrue failed", SourceCode: "LoginTests.swift:42"}},
		},
		{
			Identifier: "App/CheckoutTests/testC()", Target: "App", Suite: "CheckoutTests", Name: "testC()",
			Result: ResultSkipped, Device: "sim-2",
		},
	}
}

func writeJUnitString(t *testing.T, cases []TestCase, opts JUnitOptions) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, cases, opts); err != nil {
		t.Fatalf("WriteJUnit() error = %v", err)
	}
	return buf.String()
}

func TestWriteJUnit(t *testing.T) {
	out := writeJUnitString(t, junitCases(), JUnitOptions{
		Name:      "gxcui run",
		Timestamp: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	})

	// The document must be well-formed above all else: a CI server that cannot
	// parse it reports nothing at all.
	var parsed struct {
		XMLName  xml.Name `xml:"testsuites"`
		Tests    int      `xml:"tests,attr"`
		Failures int      `xml:"failures,attr"`
		Skipped  int      `xml:"skipped,attr"`
		Suites   []struct {
			Name     string `xml:"name,attr"`
			Tests    int    `xml:"tests,attr"`
			Failures int    `xml:"failures,attr"`
			Hostname string `xml:"hostname,attr"`
			Cases    []struct {
				Name      string `xml:"name,attr"`
				ClassName string `xml:"classname,attr"`
				Time      string `xml:"time,attr"`
				Failure   *struct {
					Message string `xml:"message,attr"`
					Type    string `xml:"type,attr"`
				} `xml:"failure"`
				Skipped *struct{} `xml:"skipped"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid XML: %v\n%s", err, out)
	}

	if parsed.Tests != 3 || parsed.Failures != 1 || parsed.Skipped != 1 {
		t.Errorf("totals = %d tests, %d failures, %d skipped; want 3, 1, 1",
			parsed.Tests, parsed.Failures, parsed.Skipped)
	}
	if len(parsed.Suites) != 2 {
		t.Fatalf("got %d suites, want 2", len(parsed.Suites))
	}

	// Suites are named Target.Class, which is what CI servers group on.
	if parsed.Suites[0].Name != "App.CheckoutTests" || parsed.Suites[1].Name != "App.LoginTests" {
		t.Errorf("suite names = %q, %q; want App.CheckoutTests, App.LoginTests",
			parsed.Suites[0].Name, parsed.Suites[1].Name)
	}
	if parsed.Suites[1].Hostname != "sim-1" {
		t.Errorf("hostname = %q, want the simulator the suite ran on", parsed.Suites[1].Hostname)
	}

	login := parsed.Suites[1]
	if login.Failures != 1 {
		t.Errorf("LoginTests failures = %d, want 1", login.Failures)
	}
	var failing bool
	for _, c := range login.Cases {
		if c.Name != "testB()" {
			continue
		}
		failing = true
		if c.Failure == nil {
			t.Fatal("testB() has no <failure> element")
		}
		if !strings.Contains(c.Failure.Message, "XCTAssertTrue failed") {
			t.Errorf("failure message = %q, want the assertion text", c.Failure.Message)
		}
		if c.Time != "2.500" {
			t.Errorf("time = %q, want a plain decimal 2.500", c.Time)
		}
	}
	if !failing {
		t.Error("testB() is missing from the report")
	}
}

// Durations must never be locale-formatted: a comma decimal separator would
// make the whole document unparseable to a CI server.
func TestWriteJUnitFormatsDurationsWithADot(t *testing.T) {
	out := writeJUnitString(t, junitCases(), JUnitOptions{})

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "time=\"") && strings.Contains(line, ",") {
			t.Errorf("duration uses a comma: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, `time="1.250"`) {
		t.Errorf("expected time=\"1.250\" in output:\n%s", out)
	}
}

// A test that ran twice and failed both times is broken, not flaky.
func TestWriteJUnitOnlyLabelsRealFlakes(t *testing.T) {
	cases := junitCases()
	opts := JUnitOptions{
		Attempts: map[string]int{
			"App/LoginTests/testA()": 2,
			"App/LoginTests/testB()": 2,
		},
		Flaky: map[string]bool{"App/LoginTests/testA()": true},
	}

	out := writeJUnitString(t, cases, opts)

	if !strings.Contains(out, "gxcui: ran 2 times (flaky: passed after failing)") {
		t.Errorf("the genuinely flaky test is not labelled:\n%s", out)
	}
	// The still-failing test is noted as retried, but not called flaky.
	if strings.Count(out, "flaky") != 1 {
		t.Errorf("a test that failed every attempt was labelled flaky:\n%s", out)
	}
	if !strings.Contains(out, "<system-out>gxcui: ran 2 times</system-out>") {
		t.Errorf("the retried failing test is not noted:\n%s", out)
	}
}

func TestWriteJUnitEmpty(t *testing.T) {
	out := writeJUnitString(t, nil, JUnitOptions{})

	var parsed struct {
		XMLName xml.Name `xml:"testsuites"`
		Tests   int      `xml:"tests,attr"`
	}
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("empty report is not valid XML: %v\n%s", err, out)
	}
	if parsed.Tests != 0 {
		t.Errorf("Tests = %d, want 0", parsed.Tests)
	}
}

func TestSuiteName(t *testing.T) {
	tests := []struct {
		c    TestCase
		want string
	}{
		{TestCase{Target: "App", Suite: "LoginTests"}, "App.LoginTests"},
		{TestCase{Target: "App", Suite: "Outer/Inner"}, "App.Outer.Inner"},
		{TestCase{Target: "App"}, "App"},
		{TestCase{Suite: "LoginTests"}, "LoginTests"},
		{TestCase{}, "gxcui"},
	}
	for _, tt := range tests {
		if got := suiteName(tt.c); got != tt.want {
			t.Errorf("suiteName(%+v) = %q, want %q", tt.c, got, tt.want)
		}
	}
}
