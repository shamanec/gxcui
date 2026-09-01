package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shamanec/gxcui/executor"
	"github.com/shamanec/gxcui/reporter"
	"github.com/spf13/cobra"
)

func newReportCommand() *cobra.Command {
	var (
		output      string
		activities  string
		attachments string
		maxSizeMB   int
		title       string
		coverage    bool
	)

	cmd := &cobra.Command{
		Use:   "report <path>",
		Short: "Write an HTML report for a result bundle",
		Long: "Report renders an .xcresult bundle as a single self-contained HTML file, with\n" +
			"its screenshots and screen recordings embedded so the report can be archived or\n" +
			"attached to a bug on its own.\n\n" +
			"The path is either an .xcresult bundle or a gxcui run directory, in which case\n" +
			"the merged bundle inside it is used. A run directory also supplies the retry and\n" +
			"flakiness information that the bundle alone does not carry.\n\n" +
			"`gxcui run` writes this report already; this command is for bundles produced\n" +
			"elsewhere, or for re-rendering one with more detail than the run was configured\n" +
			"to collect.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bundles, manifest, err := resolveBundles(args[0])
			if err != nil {
				return err
			}
			if output == "" {
				output = filepath.Join(args[0], "report.html")
				if strings.HasSuffix(args[0], ".xcresult") {
					output = filepath.Join(filepath.Dir(args[0]), "report.html")
				}
			}

			opts := reporter.HTMLOptions{
				Title:              title,
				Activities:         reporter.Detail(activities),
				Attachments:        reporter.Detail(attachments),
				MaxAttachmentBytes: int64(maxSizeMB) << 20,
				Coverage:           coverage,
				Generator:          "gxcui " + version,
			}
			if manifest != "" {
				if result, err := executor.LoadManifest(manifest); err == nil {
					opts.Attempts, opts.Flaky = retryInfo(result)
					// A merged bundle's own window covers only its first input,
					// so the run's record is the one to believe.
					opts.StartTime, opts.FinishTime = result.StartedAt, result.FinishedAt
					if opts.Title == "" {
						opts.Title = "gxcui " + result.ID
					}
				}
			}

			if err := reporter.New().WriteHTMLFromBundles(cmd.Context(), bundles, output, opts); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), output)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "", "where to write the report (default: report.html beside the bundle)")
	f.StringVar(&activities, "activities", string(reporter.DetailFailed), "include step-by-step logs for: none, failed or all")
	f.StringVar(&attachments, "attachments", string(reporter.DetailFailed), "embed screenshots and recordings for: none, failed or all")
	f.IntVar(&maxSizeMB, "max-attachment-size", 0, "skip attachments larger than this many MB (0: no limit)")
	f.StringVar(&title, "title", "", "report title (default: the run title recorded in the bundle)")
	f.BoolVar(&coverage, "coverage", false, "include code coverage, when the bundles hold any")
	return cmd
}

// resolveBundles works out which result bundles to report on, and where the run
// manifest is if there is one.
//
// The path may be a single .xcresult, a directory holding several of them, or a
// gxcui run directory. A run directory prefers its per-batch bundles over the
// merged one: each per-batch bundle records the simulator it ran on and its own
// time window, and `xcresulttool merge` keeps neither.
func resolveBundles(path string) (bundles []string, manifest string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("%s is not a result bundle", path)
	}

	if strings.HasSuffix(path, ".xcresult") {
		return []string{path}, manifestBeside(filepath.Dir(path)), nil
	}

	// A gxcui run directory.
	if batches, err := bundlesIn(filepath.Join(path, "batches")); err == nil && len(batches) > 0 {
		return batches, manifestBeside(path), nil
	}
	// Its batches may have been cleaned up after merging.
	if merged := filepath.Join(path, "merged.xcresult"); isDir(merged) {
		return []string{merged}, manifestBeside(path), nil
	}
	// Or any other directory of bundles.
	found, err := bundlesIn(path)
	if err != nil {
		return nil, "", err
	}
	if len(found) == 0 {
		return nil, "", fmt.Errorf("%s holds no .xcresult bundles", path)
	}
	return found, manifestBeside(path), nil
}

// bundlesIn lists the result bundles directly inside dir, in name order so that
// batch-01 is read before batch-02 and the report is reproducible.
func bundlesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var bundles []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".xcresult") {
			bundles = append(bundles, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(bundles)
	return bundles, nil
}

func manifestBeside(dir string) string {
	path := filepath.Join(dir, "run.json")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// retryInfo extracts what a run knows and a result bundle does not: how many
// times each test ran, and which ones only passed on a later attempt.
func retryInfo(result *executor.RunResult) (map[string]int, map[string]bool) {
	attempts := make(map[string]int, len(result.Tests))
	flaky := map[string]bool{}
	for _, test := range result.Tests {
		attempts[test.Identifier] = len(test.Attempts)
		if test.Flaky {
			flaky[test.Identifier] = true
		}
	}
	return attempts, flaky
}
