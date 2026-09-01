package reporter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

// scriptedRunner answers by inspecting the command rather than by position, so
// a coverage read — which interleaves xcresulttool and xccov calls whose order
// depends on the bundles — can be scripted without pinning the order.
type scriptedRunner struct {
	mu     sync.Mutex
	calls  []exec.Command
	handle func(exec.Command) (string, error)
}

func (r *scriptedRunner) Run(_ context.Context, cmd exec.Command) (*exec.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()

	out, err := r.handle(cmd)
	if err != nil {
		return nil, err
	}
	return &exec.Result{Stdout: out}, nil
}

func (r *scriptedRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.String())
	}
	return out
}

// find returns the first recorded command containing every one of parts.
func (r *scriptedRunner) find(parts ...string) (exec.Command, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		line := c.String()
		matched := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			return c, true
		}
	}
	return exec.Command{}, false
}

func argAfter(cmd exec.Command, flag string) string {
	for i, a := range cmd.Args {
		if a == flag && i+1 < len(cmd.Args) {
			return cmd.Args[i+1]
		}
	}
	return ""
}

const coverageJSON = `{
  "coveredLines": 30,
  "executableLines": 100,
  "lineCoverage": 0.3,
  "targets": [
    {"name": "App.app", "coveredLines": 30, "executableLines": 100, "lineCoverage": 0.3,
     "files": [
       {"name": "Big.swift", "path": "/src/Big.swift", "coveredLines": 10, "executableLines": 90, "lineCoverage": 0.111},
       {"name": "Small.swift", "path": "/src/Small.swift", "coveredLines": 20, "executableLines": 10, "lineCoverage": 1}
     ]}
  ]
}`

const availabilityYes = `{"hasCoverage": true, "hasDiagnostics": true, "hasTestResults": true}`
const availabilityNo = `{"hasCoverage": false, "hasDiagnostics": true, "hasTestResults": true}`

// A lone bundle needs no merging: xccov reads coverage out of an .xcresult
// directly, and exporting first would be pure cost.
func TestReadCoverageReadsASingleBundleDirectly(t *testing.T) {
	runner := &scriptedRunner{handle: func(cmd exec.Command) (string, error) {
		switch {
		case strings.Contains(cmd.String(), "content-availability"):
			return availabilityYes, nil
		case strings.Contains(cmd.String(), "xccov"):
			return coverageJSON, nil
		}
		return "", errors.New("unexpected command: " + cmd.String())
	}}

	coverage, err := NewWithRunner(runner).ReadCoverage(context.Background(), []string{"a.xcresult"})
	if err != nil {
		t.Fatalf("ReadCoverage() error = %v", err)
	}
	if coverage.CoveredLines != 30 || coverage.ExecutableLines != 100 {
		t.Errorf("coverage = %d/%d, want 30/100", coverage.CoveredLines, coverage.ExecutableLines)
	}

	// The flags have to precede the path: xccov rejects the other order.
	want := "xcrun xccov view --report --json a.xcresult"
	if _, ok := runner.find(want); !ok {
		t.Errorf("commands = %v, want one to be %q", runner.commands(), want)
	}
	for _, line := range runner.commands() {
		if strings.Contains(line, "export coverage") || strings.Contains(line, "xccov merge") {
			t.Errorf("a single bundle was merged: %s", line)
		}
	}
}

// Several bundles must be unioned. Adding their covered-line counts would
// double-count every line more than one batch touched.
func TestReadCoverageUnionsSeveralBundles(t *testing.T) {
	runner := &scriptedRunner{}
	runner.handle = func(cmd exec.Command) (string, error) {
		line := cmd.String()
		switch {
		case strings.Contains(line, "content-availability"):
			return availabilityYes, nil
		case strings.Contains(line, "export coverage"):
			dir := argAfter(cmd, "--output-path")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			// The real tool names these after the device it ran on.
			for _, name := range []string{"0_Test_iPhone 16_CoverageReport", "0_Test_iPhone 16_CoverageArchive"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
					return "", err
				}
			}
			return "", nil
		case strings.Contains(line, "xccov merge"):
			return "", os.WriteFile(argAfter(cmd, "--outReport"), []byte("merged"), 0o644)
		case strings.Contains(line, "xccov view"):
			return coverageJSON, nil
		}
		return "", errors.New("unexpected command: " + line)
	}

	coverage, err := NewWithRunner(runner).ReadCoverage(context.Background(), []string{"a.xcresult", "b.xcresult"})
	if err != nil {
		t.Fatalf("ReadCoverage() error = %v", err)
	}
	if coverage.ExecutableLines != 100 {
		t.Errorf("executable lines = %d, want 100", coverage.ExecutableLines)
	}

	merge, ok := runner.find("xccov merge")
	if !ok {
		t.Fatalf("no merge was run: %v", runner.commands())
	}
	// Both bundles' pairs must reach the merge, or the union silently covers
	// less than the run did.
	if got := strings.Count(merge.String(), "CoverageReport"); got != 2 {
		t.Errorf("merge got %d coverage reports, want 2: %s", got, merge)
	}
	if got := strings.Count(merge.String(), "CoverageArchive"); got != 2 {
		t.Errorf("merge got %d coverage archives, want 2: %s", got, merge)
	}
}

// xccov merge writes merged.xccovarchive into the working directory when it is
// not told otherwise, and then refuses to run at all if one is already there.
// Left alone it drops that directory into whatever the caller's cwd happens to
// be — for gxcui, the user's project.
func TestReadCoverageNeverWritesIntoTheCallersDirectory(t *testing.T) {
	runner := &scriptedRunner{}
	runner.handle = func(cmd exec.Command) (string, error) {
		line := cmd.String()
		switch {
		case strings.Contains(line, "content-availability"):
			return availabilityYes, nil
		case strings.Contains(line, "export coverage"):
			dir := argAfter(cmd, "--output-path")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			for _, name := range []string{"r_CoverageReport", "r_CoverageArchive"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
					return "", err
				}
			}
			return "", nil
		case strings.Contains(line, "xccov merge"):
			return "", os.WriteFile(argAfter(cmd, "--outReport"), []byte("merged"), 0o644)
		}
		return coverageJSON, nil
	}

	if _, err := NewWithRunner(runner).ReadCoverage(context.Background(), []string{"a.xcresult", "b.xcresult"}); err != nil {
		t.Fatalf("ReadCoverage() error = %v", err)
	}

	merge, ok := runner.find("xccov merge")
	if !ok {
		t.Fatal("no merge was run")
	}
	archive := argAfter(merge, "--outArchive")
	if archive == "" {
		t.Fatal("merge did not pass --outArchive, so xccov writes one into the working directory")
	}
	if merge.Dir == "" {
		t.Error("merge ran in the caller's working directory")
	}
	if !strings.HasPrefix(archive, merge.Dir) {
		t.Errorf("--outArchive %q is outside the temporary directory %q", archive, merge.Dir)
	}
}

// A bundle whose scheme never gathered coverage has nothing to contribute and
// must not drag the others down or fail the read.
func TestReadCoverageSkipsBundlesWithoutCoverage(t *testing.T) {
	runner := &scriptedRunner{}
	runner.handle = func(cmd exec.Command) (string, error) {
		line := cmd.String()
		switch {
		case strings.Contains(line, "content-availability"):
			if strings.Contains(line, "bare.xcresult") {
				return availabilityNo, nil
			}
			return availabilityYes, nil
		case strings.Contains(line, "xccov"):
			return coverageJSON, nil
		}
		return "", errors.New("unexpected command: " + line)
	}

	if _, err := NewWithRunner(runner).ReadCoverage(context.Background(), []string{"bare.xcresult", "full.xcresult"}); err != nil {
		t.Fatalf("ReadCoverage() error = %v", err)
	}
	// Only one bundle had coverage, so it is read directly rather than merged.
	if _, ok := runner.find("xccov view --report --json full.xcresult"); !ok {
		t.Errorf("commands = %v, want the covered bundle read directly", runner.commands())
	}
	if _, ok := runner.find("export coverage"); ok {
		t.Error("exported coverage despite only one bundle having any")
	}
}

// Running without coverage gathering is normal, not a failure.
func TestReadCoverageWithoutAnyIsErrNoCoverage(t *testing.T) {
	runner := &scriptedRunner{handle: func(exec.Command) (string, error) { return availabilityNo, nil }}

	_, err := NewWithRunner(runner).ReadCoverage(context.Background(), []string{"a.xcresult", "b.xcresult"})
	if !errors.Is(err, ErrNoCoverage) {
		t.Errorf("error = %v, want ErrNoCoverage", err)
	}
}

// The exported names carry the device, so a report can only be matched to its
// archive by sorting; pairing them by position in the glob would cross them
// over as soon as a bundle held more than one.
func TestCoveragePairsMatchEachReportToItsArchive(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"0_Test_iPhone 16_CoverageReport", "0_Test_iPhone 16_CoverageArchive",
		"1_Test_iPad Pro_CoverageReport", "1_Test_iPad Pro_CoverageArchive",
		"unrelated.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pairs := coveragePairs(dir)
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want 2: %v", len(pairs), pairs)
	}
	for _, pair := range pairs {
		report, archive := filepath.Base(pair[0]), filepath.Base(pair[1])
		if strings.TrimSuffix(report, "Report") != strings.TrimSuffix(archive, "Archive") {
			t.Errorf("report %q was paired with archive %q", report, archive)
		}
	}
}

func TestHTMLCoverageRanksFilesByUncoveredLines(t *testing.T) {
	coverage := htmlCoverage(&Coverage{
		CoveredLines: 30, ExecutableLines: 100,
		Targets: []CoverageTarget{{
			Name: "App.app", CoveredLines: 30, ExecutableLines: 100,
			Files: []CoverageFile{
				{Name: "Small.swift", CoveredLines: 0, ExecutableLines: 10},
				{Name: "Big.swift", CoveredLines: 10, ExecutableLines: 90},
			},
		}},
	})

	files := coverage.Targets[0].Files
	// Big.swift is at 11% with 80 lines untested; Small.swift is at 0% with 10.
	// Worst-percentage-first would put the wrong one on top: the question is
	// where the untested code is, not which file scores lowest.
	if files[0].Name != "Big.swift" {
		t.Errorf("files ranked %q first, want Big.swift", files[0].Name)
	}
	if files[0].UncoveredLines() != 80 {
		t.Errorf("uncovered = %d, want 80", files[0].UncoveredLines())
	}
}

// A target with nothing executable in it is an artefact of linking. A row
// reading "0.00% (0/0)" states a fact about no code at all.
func TestHTMLCoverageDropsTargetsWithNothingInThem(t *testing.T) {
	coverage := htmlCoverage(&Coverage{
		ExecutableLines: 100,
		Targets: []CoverageTarget{
			{Name: "Resources.bundle", CoveredLines: 0, ExecutableLines: 0},
			{Name: "App.app", CoveredLines: 30, ExecutableLines: 100},
		},
	})

	if len(coverage.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %v", len(coverage.Targets), coverage.Targets)
	}
	if coverage.Targets[0].Name != "App.app" {
		t.Errorf("kept %q, want App.app", coverage.Targets[0].Name)
	}
	if got := coverage.Targets[0].Percent; got != 30 {
		t.Errorf("percent = %v, want 30", got)
	}

	// Nothing left to say means no section at all, rather than an empty one.
	if htmlCoverage(&Coverage{Targets: []CoverageTarget{{Name: "Empty"}}}) != nil {
		t.Error("a report with no executable lines still produced a coverage section")
	}
}

func TestRenderHTMLShowsCoverage(t *testing.T) {
	report := &HTMLReport{
		Title: "Test", Result: ResultPassed,
		Coverage: htmlCoverage(&Coverage{
			CoveredLines: 30, ExecutableLines: 100,
			Targets: []CoverageTarget{{
				Name: "App.app", CoveredLines: 30, ExecutableLines: 100,
				Files: []CoverageFile{{Name: "Big.swift", Path: "/src/Big.swift", CoveredLines: 10, ExecutableLines: 90}},
			}},
		}),
	}

	var buf bytes.Buffer
	if err := RenderHTML(&buf, report); err != nil {
		t.Fatalf("RenderHTML() error = %v", err)
	}
	html := buf.String()

	for _, want := range []string{"Code coverage", "App.app", "30.00%", "Big.swift", "11.11%"} {
		if !strings.Contains(html, want) {
			t.Errorf("report does not mention %q", want)
		}
	}
	// The bar's width is an inline style, which html/template blanks to
	// ZgotmplZ unless it is handed a value typed as CSS.
	if !strings.Contains(html, "width: 30.00%") {
		t.Error("the coverage bar has no width")
	}
	if strings.Contains(html, "ZgotmplZ") {
		t.Error("html/template rejected a value as unsafe")
	}
}

// The section is opt-in: a report that did not ask for coverage must not pay
// for it, and must not show an empty heading either.
func TestRenderHTMLOmitsCoverageWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHTML(&buf, &HTMLReport{Title: "Test", Result: ResultPassed}); err != nil {
		t.Fatalf("RenderHTML() error = %v", err)
	}
	if strings.Contains(buf.String(), "Code coverage") {
		t.Error("a report without coverage still has a coverage section")
	}
}

func TestBuildHTMLDoesNotReadCoverageUnlessAsked(t *testing.T) {
	runner := &scriptedRunner{handle: func(cmd exec.Command) (string, error) {
		return "", errors.New("unexpected command: " + cmd.String())
	}}
	if _, err := NewWithRunner(runner).ReadCoverage(context.Background(), nil); !errors.Is(err, ErrNoCoverage) {
		t.Errorf("error = %v, want ErrNoCoverage for no bundles", err)
	}
	if len(runner.commands()) != 0 {
		t.Errorf("ran %v for no bundles", runner.commands())
	}
}
