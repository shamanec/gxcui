package reporter

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JUnitOptions tunes the JUnit XML output.
type JUnitOptions struct {
	// Name labels the whole run in the <testsuites> element.
	Name string
	// Timestamp is the run's start time.
	Timestamp time.Time
	// Attempts maps a test identifier to how many times it ran.
	Attempts map[string]int
	// Flaky marks the tests that passed only after failing. A test that ran
	// several times and failed every time is not flaky, it is broken.
	Flaky map[string]bool
}

type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Name     string           `xml:"name,attr,omitempty"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Skipped  int              `xml:"skipped,attr"`
	Time     string           `xml:"time,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name       string           `xml:"name,attr"`
	Tests      int              `xml:"tests,attr"`
	Failures   int              `xml:"failures,attr"`
	Skipped    int              `xml:"skipped,attr"`
	Time       string           `xml:"time,attr"`
	Timestamp  string           `xml:"timestamp,attr,omitempty"`
	Hostname   string           `xml:"hostname,attr,omitempty"`
	Properties *junitProperties `xml:"properties,omitempty"`
	Cases      []junitTestCase  `xml:"testcase"`
}

type junitProperties struct {
	Properties []junitProperty `xml:"property"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
}

// WriteJUnit renders test results as JUnit XML.
//
// Tests are grouped into one <testsuite> per class, named "Target.Class", which
// is the convention CI servers expect. The device a suite ran on is recorded
// both as the suite's hostname and as a property, since gxcui spreads a run
// across several simulators and that context is otherwise lost.
func WriteJUnit(w io.Writer, cases []TestCase, opts JUnitOptions) error {
	suites := map[string][]TestCase{}
	var order []string
	for _, c := range cases {
		key := suiteName(c)
		if _, seen := suites[key]; !seen {
			order = append(order, key)
		}
		suites[key] = append(suites[key], c)
	}
	sort.Strings(order)

	doc := junitTestSuites{Name: opts.Name}
	for _, name := range order {
		suite := buildSuite(name, suites[name], opts)
		doc.Suites = append(doc.Suites, suite)
		doc.Tests += suite.Tests
		doc.Failures += suite.Failures
		doc.Skipped += suite.Skipped
	}

	var total float64
	for _, c := range cases {
		total += c.Duration
	}
	doc.Time = formatSeconds(total)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("write junit: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("write junit: %w", err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("write junit: %w", err)
	}
	return nil
}

func buildSuite(name string, cases []TestCase, opts JUnitOptions) junitTestSuite {
	suite := junitTestSuite{Name: name}
	if !opts.Timestamp.IsZero() {
		suite.Timestamp = opts.Timestamp.Format(time.RFC3339)
	}

	devices := map[string]bool{}
	var elapsed float64

	for _, c := range cases {
		if c.Device != "" {
			devices[c.Device] = true
		}
		elapsed += c.Duration

		tc := junitTestCase{
			Name:      c.Name,
			ClassName: name,
			Time:      formatSeconds(c.Duration),
		}

		switch {
		case c.Result.Failed():
			tc.Failure = &junitFailure{
				Message: failureSummary(c.Failures),
				Type:    "XCTestFailure",
				Text:    failureDetail(c),
			}
			suite.Failures++
		case c.Result == ResultSkipped:
			tc.Skipped = &junitSkipped{}
			suite.Skipped++
		}

		if attempts := opts.Attempts[c.Identifier]; attempts > 1 {
			// The classic JUnit schema has no flakiness concept, and inventing
			// elements breaks strict parsers. Recording it as output keeps the
			// document valid while preserving the information.
			note := fmt.Sprintf("gxcui: ran %d times", attempts)
			if opts.Flaky[c.Identifier] {
				note += " (flaky: passed after failing)"
			}
			tc.SystemOut = note
		}
		suite.Cases = append(suite.Cases, tc)
		suite.Tests++
	}

	suite.Time = formatSeconds(elapsed)
	if len(devices) > 0 {
		names := make([]string, 0, len(devices))
		for d := range devices {
			names = append(names, d)
		}
		sort.Strings(names)
		if len(names) == 1 {
			suite.Hostname = names[0]
		}
		suite.Properties = &junitProperties{Properties: []junitProperty{
			{Name: "gxcui.devices", Value: strings.Join(names, ", ")},
		}}
	}
	return suite
}

// WriteJUnitFile renders JUnit XML to a file, creating parent directories.
func WriteJUnitFile(path string, cases []TestCase, opts JUnitOptions) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write junit: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write junit: %w", err)
	}
	defer f.Close()

	if err := WriteJUnit(f, cases, opts); err != nil {
		return err
	}
	return f.Close()
}

// suiteName renders the JUnit class name for a test, "Target.Class".
func suiteName(c TestCase) string {
	suite := strings.ReplaceAll(c.Suite, "/", ".")
	switch {
	case c.Target == "" && suite == "":
		return "gxcui"
	case c.Target == "":
		return suite
	case suite == "":
		return c.Target
	}
	return c.Target + "." + suite
}

func failureSummary(failures []Failure) string {
	for _, f := range failures {
		if f.Message != "" {
			return truncate(firstLine(f.Message), 500)
		}
	}
	return "test failed"
}

func failureDetail(c TestCase) string {
	var b strings.Builder
	for _, f := range c.Failures {
		if f.Message != "" {
			b.WriteString(f.Message)
			b.WriteString("\n")
		}
		if f.SourceCode != "" {
			b.WriteString("  at ")
			b.WriteString(f.SourceCode)
			b.WriteString("\n")
		}
	}
	if c.Device != "" {
		fmt.Fprintf(&b, "  on %s\n", c.Device)
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// formatSeconds renders a duration the way JUnit consumers expect: a plain
// decimal number of seconds, always with a dot, never locale-formatted.
func formatSeconds(seconds float64) string {
	return fmt.Sprintf("%.3f", seconds)
}
