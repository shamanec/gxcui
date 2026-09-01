package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/shamanec/gxcui/executor"
	"github.com/spf13/cobra"
)

// Exit codes. CI needs to tell "the tests are broken" apart from "gxcui could
// not run them", because only one of those is the author's problem.
const (
	exitOK       = 0
	exitTestFail = 1
	exitError    = 2
)

// errTestsFailed signals a completed run with failing tests, as opposed to a
// run that could not happen.
var errTestsFailed = errors.New("tests failed")

func newRunCommand(global *globalFlags) *cobra.Command {
	var (
		dryRun        bool
		strategy      string
		batches       int
		attempts      int
		outDir        string
		bootSims      bool
		resetBefore   bool
		shutdownAfter bool
		coverage      bool
		noReport      bool
		noHTML        bool
		quiet         bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the tests in parallel across booted simulators",
		Long: "Run builds the project if needed, discovers its tests, splits them into batches\n" +
			"and runs each batch on a different booted simulator.\n\n" +
			"With simulators.bootSims, or --boot-sims, the named simulators are booted first,\n" +
			"all at once, and the run waits for them. Adding simulators.resetBefore erases them\n" +
			"first so they come up clean, and simulators.shutdownAfter releases them when the\n" +
			"run is done.\n\n" +
			"Each batch writes its own result bundle and log. When the run finishes, the\n" +
			"bundles are merged into one, a JUnit report is written, and per-test durations\n" +
			"are recorded so the next run can balance its batches better.\n\n" +
			"Interrupting with Ctrl-C stops the batches in flight but still writes reports\n" +
			"for everything that finished.\n\n" +
			"Exit codes: 0 all passed, 1 tests failed, 2 gxcui could not complete the run.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := global.load(true)
			if err != nil {
				return err
			}
			if strategy != "" {
				cfg.Batching.Strategy = executor.Strategy(strategy)
			}
			if batches > 0 {
				cfg.Batching.Batches = batches
			}
			if attempts > 0 {
				cfg.Retries.MaxAttempts = attempts
			}
			if outDir != "" {
				cfg.Output.Dir = outDir
			}
			// A bool flag cannot be told "unset" from "false", so only an
			// explicitly given --boot-sims overrides the config file.
			if cmd.Flags().Changed("boot-sims") {
				cfg.Simulators.BootSims = bootSims
			}
			if cmd.Flags().Changed("reset-before") {
				cfg.Simulators.ResetBefore = resetBefore
			}
			if cmd.Flags().Changed("shutdown-after") {
				cfg.Simulators.ShutdownAfter = shutdownAfter
			}
			if cmd.Flags().Changed("coverage") {
				cfg.Output.HTML.Coverage = coverage
			}
			if noHTML {
				cfg.Output.HTML.Enabled = executor.Off()
			}
			if noReport {
				cfg.Output.Merge, cfg.Output.JUnit = executor.Off(), executor.Off()
				cfg.Output.HTML.Enabled = executor.Off()
			}
			if err := cfg.Validate(); err != nil {
				return err
			}

			exec := executor.New(cfg)
			out := cmd.OutOrStdout()

			if dryRun {
				plan, err := exec.DryRun(cmd.Context(), executor.RunOptions{})
				if err != nil {
					return err
				}
				return printPlan(out, plan)
			}

			printer := newProgressPrinter(out, !quiet && isTTY(out))
			defer printer.finish()

			opts := executor.RunOptions{Progress: printer.handle}
			if quiet {
				opts.Progress = nil
			}

			result, runErr := exec.Run(cmd.Context(), opts)
			printer.finish()
			if result == nil {
				return runErr
			}

			printSummary(out, result)
			if runErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", runErr)
			}
			if !result.Summary.OK() || result.Interrupted {
				return errTestsFailed
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "print the batch plan and the commands, run nothing")
	f.StringVar(&strategy, "strategy", "", "batching strategy: duration, class, count or shard")
	f.IntVar(&batches, "batches", 0, "number of batches (default: two per simulator)")
	f.IntVar(&attempts, "attempts", 0, "how many times a failing test may run in total")
	f.StringVar(&outDir, "output-dir", "", "where to write run directories")
	f.BoolVar(&bootSims, "boot-sims", false, "boot the simulators named by --simulator or simulators.include before running")
	f.BoolVar(&resetBefore, "reset-before", false, "shut down and erase those simulators before running; needs --boot-sims to bring them back up")
	f.BoolVar(&shutdownAfter, "shutdown-after", false, "shut those simulators down once the last batch has finished")
	f.BoolVar(&coverage, "coverage", false, "include code coverage in the HTML report, when the run gathered any")
	f.BoolVar(&noReport, "no-report", false, "skip merging result bundles and writing the JUnit and HTML reports")
	f.BoolVar(&noHTML, "no-html", false, "skip the HTML report, which is the slowest one to produce")
	f.BoolVarP(&quiet, "quiet", "q", false, "only print the final summary")
	return cmd
}

func printPlan(w io.Writer, plan *executor.RunPlan) error {
	fmt.Fprintf(w, "xctestrun: %s\n", plan.XCTestRun)
	fmt.Fprintf(w, "strategy:  %s\n", plan.Strategy)

	if len(plan.Reset) > 0 {
		fmt.Fprintf(w, "reset:     %s\n", strings.Join(plan.Reset, ", "))
	}
	if len(plan.Boot) > 0 {
		fmt.Fprintf(w, "boot:      %s\n", strings.Join(plan.Boot, ", "))
	}
	if len(plan.Shutdown) > 0 {
		fmt.Fprintf(w, "shutdown:  %s\n", strings.Join(plan.Shutdown, ", "))
	}

	fmt.Fprintf(w, "\nsimulators (%d):\n", len(plan.Devices))
	for _, d := range plan.Devices {
		fmt.Fprintf(w, "  %s\n", d)
	}

	fmt.Fprintf(w, "\nbatches (%d):\n", len(plan.Batches))
	for _, b := range plan.Batches {
		fmt.Fprintf(w, "  %s — %d test(s), ~%.0fs\n", b.ID, b.Size(), b.EstimatedSeconds)
		for _, test := range b.Tests {
			fmt.Fprintf(w, "      %s\n", test)
		}
	}

	fmt.Fprintf(w, "\ncommands:\n")
	for _, c := range plan.Commands {
		fmt.Fprintf(w, "  %s\n", c)
	}
	return nil
}

// printSummary is the last thing a run prints: what failed, and how to run it
// again. It is deliberately the most detailed output gxcui produces, because it
// is the part anyone actually reads.
func printSummary(w io.Writer, r *executor.RunResult) {
	s := r.Summary

	fmt.Fprintf(w, "\n%s\n", strings.Repeat("─", 60))
	if r.Interrupted {
		fmt.Fprintf(w, "INTERRUPTED — reporting on what finished\n")
	}
	fmt.Fprintf(w, "%d test(s) in %.0fs on %d simulator(s): %d passed",
		s.Total, r.Seconds, len(r.Devices), s.Passed)
	if s.Failed > 0 {
		fmt.Fprintf(w, ", %d failed", s.Failed)
	}
	if s.Skipped > 0 {
		fmt.Fprintf(w, ", %d skipped", s.Skipped)
	}
	if s.Flaky > 0 {
		fmt.Fprintf(w, ", %d flaky", s.Flaky)
	}
	if s.Unaccounted > 0 {
		fmt.Fprintf(w, ", %d unaccounted", s.Unaccounted)
	}
	fmt.Fprintln(w)

	if failed := r.FailedTests(); len(failed) > 0 {
		fmt.Fprintf(w, "\nFailed (%d):\n", len(failed))
		for _, t := range failed {
			fmt.Fprintf(w, "  ✗ %s", t.Identifier)
			if device := t.Device(); device != "" {
				fmt.Fprintf(w, "  (%s)", device)
			}
			fmt.Fprintln(w)
			for _, msg := range t.Failures() {
				fmt.Fprintf(w, "      %s\n", firstLine(msg))
			}
		}
	}

	if unknown := r.UnaccountedTests(); len(unknown) > 0 {
		fmt.Fprintf(w, "\nNo result reported (%d) — the batch running these did not finish:\n", len(unknown))
		for _, t := range unknown {
			fmt.Fprintf(w, "  ? %s\n", t.Identifier)
		}
	}

	if flaky := r.FlakyTests(); len(flaky) > 0 {
		fmt.Fprintf(w, "\nFlaky (%d) — passed only after retrying:\n", len(flaky))
		for _, t := range flaky {
			fmt.Fprintf(w, "  ~ %s (%d attempts)\n", t.Identifier, len(t.Attempts))
		}
	}

	fmt.Fprintf(w, "\nArtifacts:\n")
	for _, artifact := range []struct{ label, path string }{
		{"report", r.Artifacts.HTML},
		{"results", r.Artifacts.MergedBundle},
		{"junit", r.Artifacts.JUnit},
		{"manifest", r.Artifacts.Manifest},
		{"logs", r.Artifacts.Logs},
	} {
		if artifact.path != "" {
			fmt.Fprintf(w, "  %-9s %s\n", artifact.label, artifact.path)
		}
	}
	if r.Artifacts.HTML == "" && r.Artifacts.MergedBundle != "" {
		fmt.Fprintf(w, "\nHTML report:\n  gxcui report %s\n", r.Artifacts.MergedBundle)
	}

	if rerun := r.RerunCommand(); rerun != "" {
		fmt.Fprintf(w, "\nRe-run just these:\n  %s\n", rerun)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// exitCodeFor maps an error from a command to a process exit status.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errTestsFailed):
		return exitTestFail
	case errors.Is(err, context.Canceled):
		return exitTestFail
	default:
		return exitError
	}
}
