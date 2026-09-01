package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shamanec/gxcui/reporter"
)

// report writes everything a finished run leaves behind: the merged result
// bundle, the JUnit report, updated timings and the run manifest.
//
// Failures here are collected rather than returned immediately, so one broken
// output (a merge that fails, say) does not cost the others.
func (e *Executor) report(ctx context.Context, prep *preparation, result *RunResult, opts RunOptions) error {
	var problems []string

	byIdentifier := map[string]reporter.TestCase{}
	var bundles []string
	for _, br := range result.Batches {
		for _, c := range br.cases {
			byIdentifier[c.Identifier] = c
		}
		if br.Status == BatchCompleted && br.ResultBundle != "" {
			bundles = append(bundles, br.ResultBundle)
		}
	}
	cases := finalCases(result.Tests, byIdentifier)

	rep := reporter.NewWithRunner(e.runner)

	if e.cfg.Output.Merge.Enabled() && len(bundles) > 0 {
		merged := filepath.Join(prep.dirs.root, "merged.xcresult")
		if path, err := rep.Merge(ctx, bundles, merged); err != nil {
			problems = append(problems, err.Error())
		} else {
			result.Artifacts.MergedBundle = path
		}
	}

	if e.cfg.Output.JUnit.Enabled() {
		path := filepath.Join(prep.dirs.root, "junit.xml")
		err := reporter.WriteJUnitFile(path, cases, reporter.JUnitOptions{
			Name:      "gxcui " + result.ID,
			Timestamp: result.StartedAt,
			Attempts:  attemptCounts(result.Tests),
			Flaky:     flakyTests(result.Tests),
		})
		if err != nil {
			problems = append(problems, err.Error())
		} else {
			result.Artifacts.JUnit = path
		}
	}

	// The HTML report reads the per-batch bundles as they are, rather than the
	// merge: each one knows which simulator it ran on and when, and merging
	// discards both. It does have to precede the clean-up that may delete them.
	if e.cfg.Output.HTML.Enabled.Enabled() && len(bundles) > 0 {
		path := e.cfg.Output.HTML.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(prep.dirs.root, path)
		}
		err := rep.WriteHTMLFromBundles(ctx, bundles, path, reporter.HTMLOptions{
			Title: "gxcui " + result.ID,
			// The bundles only span from the first batch to the last; the run
			// also spent time building and enumerating before that.
			StartTime:          result.StartedAt,
			FinishTime:         result.FinishedAt,
			Activities:         e.cfg.Output.HTML.Activities,
			Attachments:        e.cfg.Output.HTML.Attachments,
			MaxAttachmentBytes: int64(e.cfg.Output.HTML.MaxAttachmentSizeMB) << 20,
			Coverage:           e.cfg.Output.HTML.Coverage,
			Attempts:           attemptCounts(result.Tests),
			Flaky:              flakyTests(result.Tests),
			Generator:          "gxcui",
		})
		if err != nil {
			problems = append(problems, err.Error())
		} else {
			result.Artifacts.HTML = path
		}
	}

	if e.cfg.Output.TimingsFile != "" {
		// Only durations from completed runs are worth learning from; a test
		// killed by a timeout would poison the estimates.
		prep.timings.ObserveAll(cases, result.FinishedAt)
		if err := prep.timings.Save(e.cfg.Output.TimingsFile, result.FinishedAt); err != nil {
			problems = append(problems, err.Error())
		} else {
			result.Artifacts.Timings = e.cfg.Output.TimingsFile
		}
	}

	if !e.cfg.Output.KeepResultBundles.Enabled() && result.Artifacts.MergedBundle != "" {
		for _, bundle := range bundles {
			if err := os.RemoveAll(bundle); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}

	manifest := filepath.Join(prep.dirs.root, "run.json")
	result.Artifacts.Manifest = manifest
	if err := result.WriteManifest(manifest); err != nil {
		problems = append(problems, err.Error())
	}

	if len(problems) > 0 {
		return fmt.Errorf("writing reports: %s", strings.Join(problems, "; "))
	}
	return nil
}
