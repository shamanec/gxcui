package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// ErrNoCoverage reports that the bundles hold no coverage data at all.
//
// It is not a failure: gathering coverage is a scheme setting, and a suite run
// without it is perfectly normal. Callers are meant to leave the section out
// rather than complain.
var ErrNoCoverage = errors.New("the result bundles hold no code coverage")

// Coverage is the line coverage of a run, as `xccov ... --json` reports it.
type Coverage struct {
	CoveredLines    int              `json:"coveredLines"`
	ExecutableLines int              `json:"executableLines"`
	LineCoverage    float64          `json:"lineCoverage"`
	Targets         []CoverageTarget `json:"targets"`
}

// CoverageTarget is one built product's coverage: the app, a test bundle or a
// dependency.
type CoverageTarget struct {
	Name            string         `json:"name"`
	CoveredLines    int            `json:"coveredLines"`
	ExecutableLines int            `json:"executableLines"`
	LineCoverage    float64        `json:"lineCoverage"`
	Files           []CoverageFile `json:"files"`
}

// CoverageFile is one source file's coverage. The per-function breakdown
// alongside it in the JSON is deliberately not decoded: it is the bulk of a
// 1.5 MB document and the report has no use for it.
type CoverageFile struct {
	Name            string  `json:"name"`
	Path            string  `json:"path"`
	CoveredLines    int     `json:"coveredLines"`
	ExecutableLines int     `json:"executableLines"`
	LineCoverage    float64 `json:"lineCoverage"`
}

// ContentAvailability is the output of
// `xcresulttool get content-availability`: what a bundle actually holds.
type ContentAvailability struct {
	HasCoverage    bool `json:"hasCoverage"`
	HasDiagnostics bool `json:"hasDiagnostics"`
	HasTestResults bool `json:"hasTestResults"`
}

// ReadContentAvailability reports what a result bundle holds. It is the cheap
// way to ask whether looking for coverage is worth the work.
func (r *Reporter) ReadContentAvailability(ctx context.Context, path string) (*ContentAvailability, error) {
	out, err := r.resultTool(ctx, "get", "content-availability", "--path", path)
	if err != nil {
		return nil, fmt.Errorf("read content availability of %s: %w", path, err)
	}
	var available ContentAvailability
	if err := json.Unmarshal(out, &available); err != nil {
		return nil, fmt.Errorf("parse content availability of %s: %w", path, err)
	}
	return &available, nil
}

// ReadCoverage returns the combined line coverage of the given result bundles.
//
// Several bundles are unioned rather than summed: batches overlap, so adding
// their covered-line counts double-counts every line more than one batch
// touched and produces figures well over 100%. The union is done by exporting
// each bundle's coverage report and archive and handing the pairs to
// `xccov merge`, which is what `xcresulttool merge` does internally — merging
// the bundles first and reading the result gives identical numbers, at the cost
// of merging gigabytes of attachments to get at a few megabytes of coverage.
//
// Nothing here consults the scheme: the caller may have been handed loose
// bundles and have no project at all. Each bundle records whether coverage was
// gathered into it, which is what the availability check reads. That check is
// load-bearing rather than an optimisation — xccov fails outright on a bundle
// holding no coverage, so one such bundle in a directory would otherwise take
// the whole report down with it.
//
// When none of the bundles hold any, the error is ErrNoCoverage.
func (r *Reporter) ReadCoverage(ctx context.Context, bundlePaths []string) (*Coverage, error) {
	var withCoverage []string
	for _, path := range bundlePaths {
		available, err := r.ReadContentAvailability(ctx, path)
		if err != nil {
			return nil, err
		}
		if available.HasCoverage {
			withCoverage = append(withCoverage, path)
		}
	}
	if len(withCoverage) == 0 {
		return nil, ErrNoCoverage
	}

	// One bundle needs no merging, and xccov reads coverage straight out of an
	// .xcresult given --report.
	if len(withCoverage) == 1 {
		return r.coverageReport(ctx, withCoverage[0], "--report")
	}

	dir, err := os.MkdirTemp("", "gxcui-coverage-")
	if err != nil {
		return nil, fmt.Errorf("read coverage: %w", err)
	}
	defer os.RemoveAll(dir)

	var pairs [][2]string
	for i, path := range withCoverage {
		out := filepath.Join(dir, fmt.Sprintf("%03d", i))
		if _, err := r.resultTool(ctx, "export", "coverage", "--path", path, "--output-path", out); err != nil {
			return nil, fmt.Errorf("export coverage of %s: %w", path, err)
		}
		pairs = append(pairs, coveragePairs(out)...)
	}

	switch len(pairs) {
	case 0:
		return nil, ErrNoCoverage
	case 1:
		// xccov merge demands two or more pairs, so a lone one is read as it is.
		return r.coverageReport(ctx, pairs[0][0])
	}

	merged := filepath.Join(dir, "merged.xccovreport")
	args := []string{
		"merge",
		"--outReport", merged,
		// --outArchive is passed even though nothing reads it: without it xccov
		// writes merged.xccovarchive into the working directory, which is the
		// caller's, and then fails outright if one is already there.
		"--outArchive", filepath.Join(dir, "merged.xccovarchive"),
	}
	for _, pair := range pairs {
		args = append(args, pair[0], pair[1])
	}
	if _, err := r.xccov(ctx, dir, args...); err != nil {
		return nil, fmt.Errorf("merge coverage: %w", err)
	}
	return r.coverageReport(ctx, merged)
}

// coverageReport runs `xccov view --json` over a report file or, with --report
// among the flags, a result bundle. The flags precede the path, which is the
// only order xccov accepts.
func (r *Reporter) coverageReport(ctx context.Context, path string, flags ...string) (*Coverage, error) {
	args := append([]string{"view"}, flags...)
	args = append(args, "--json", path)

	out, err := r.xccov(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("read coverage: %w", err)
	}
	var coverage Coverage
	if err := json.Unmarshal(out, &coverage); err != nil {
		return nil, fmt.Errorf("parse coverage: %w", err)
	}
	if coverage.ExecutableLines == 0 {
		return nil, ErrNoCoverage
	}
	return &coverage, nil
}

// xccov runs an xccov subcommand from dir and returns its stdout.
func (r *Reporter) xccov(ctx context.Context, dir string, args ...string) ([]byte, error) {
	res, err := r.runner.Run(ctx, exec.Command{
		Name: "xcrun",
		Args: append([]string{"xccov"}, args...),
		Dir:  dir,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("xccov %s exited %d: %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// HTMLCoverage is the coverage section of the report, kept apart from the xccov
// schema for the same reason the rest of the model is: so the template does not
// move when the tool's JSON does.
type HTMLCoverage struct {
	CoveredLines    int
	ExecutableLines int
	Percent         float64
	Targets         []HTMLCoverageTarget
}

// HTMLCoverageTarget is one built product's coverage.
type HTMLCoverageTarget struct {
	Name            string
	CoveredLines    int
	ExecutableLines int
	Percent         float64
	Files           []HTMLCoverageFile
}

// HTMLCoverageFile is one source file's coverage.
type HTMLCoverageFile struct {
	Name            string
	Path            string
	CoveredLines    int
	ExecutableLines int
	Percent         float64
}

// UncoveredLines is what the file list is ranked by: the question a coverage
// table gets opened for is where the untested code is, not which file scores
// worst — a one-line file at 0% is not the place to start.
func (f HTMLCoverageFile) UncoveredLines() int { return f.ExecutableLines - f.CoveredLines }

// htmlCoverage converts a coverage report into the report model.
//
// Targets and files with nothing executable in them are dropped. They are an
// artefact of linking — a resource-only framework, a header-only dependency —
// and a row reading "0.00% (0/0)" states a fact about no code at all.
func htmlCoverage(c *Coverage) *HTMLCoverage {
	if c == nil {
		return nil
	}
	out := &HTMLCoverage{
		CoveredLines:    c.CoveredLines,
		ExecutableLines: c.ExecutableLines,
		Percent:         percent(c.CoveredLines, c.ExecutableLines),
	}

	for _, target := range c.Targets {
		if target.ExecutableLines == 0 {
			continue
		}
		converted := HTMLCoverageTarget{
			Name:            target.Name,
			CoveredLines:    target.CoveredLines,
			ExecutableLines: target.ExecutableLines,
			Percent:         percent(target.CoveredLines, target.ExecutableLines),
		}
		for _, file := range target.Files {
			if file.ExecutableLines == 0 {
				continue
			}
			converted.Files = append(converted.Files, HTMLCoverageFile{
				Name:            file.Name,
				Path:            file.Path,
				CoveredLines:    file.CoveredLines,
				ExecutableLines: file.ExecutableLines,
				Percent:         percent(file.CoveredLines, file.ExecutableLines),
			})
		}
		sort.SliceStable(converted.Files, func(i, j int) bool {
			a, b := converted.Files[i], converted.Files[j]
			if a.UncoveredLines() != b.UncoveredLines() {
				return a.UncoveredLines() > b.UncoveredLines()
			}
			return a.Name < b.Name
		})
		out.Targets = append(out.Targets, converted)
	}

	// Biggest target first, which on any real project puts the app above the
	// test bundle and its dependencies.
	sort.SliceStable(out.Targets, func(i, j int) bool {
		if out.Targets[i].ExecutableLines != out.Targets[j].ExecutableLines {
			return out.Targets[i].ExecutableLines > out.Targets[j].ExecutableLines
		}
		return out.Targets[i].Name < out.Targets[j].Name
	})

	if len(out.Targets) == 0 {
		return nil
	}
	return out
}

// percent renders a ratio as a percentage, recomputed from the line counts
// rather than taken from the tool's own lineCoverage so that the number always
// agrees with the "(covered/executable)" beside it.
func percent(covered, executable int) float64 {
	if executable <= 0 {
		return 0
	}
	return float64(covered) / float64(executable) * 100
}

// coveragePairs finds the report/archive pairs `xcresulttool export coverage`
// wrote into dir.
//
// The exported names carry the device they came from — "0_Test_iPhone
// 15_CoverageReport" — so they cannot be constructed, only matched. Sorting
// both sides lines each report up with the archive it belongs to, since the two
// share everything but the suffix.
func coveragePairs(dir string) [][2]string {
	reports, _ := filepath.Glob(filepath.Join(dir, "*CoverageReport"))
	archives, _ := filepath.Glob(filepath.Join(dir, "*CoverageArchive"))
	sort.Strings(reports)
	sort.Strings(archives)

	n := len(reports)
	if len(archives) < n {
		n = len(archives)
	}
	pairs := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		pairs = append(pairs, [2]string{reports[i], archives[i]})
	}
	return pairs
}
