package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/shamanec/gxcui/executor"
	"github.com/spf13/cobra"
)

// output formats shared by the commands that list things.
const (
	formatList = "list"
	formatJSON = "json"
	formatTree = "tree"
)

func newEnumerateCommand(global *globalFlags) *cobra.Command {
	var (
		format      string
		device      string
		dryRun      bool
		verbose     bool
		xcodeOutput bool
	)

	cmd := &cobra.Command{
		Use:   "enumerate",
		Short: "List the tests that would run",
		Long: "Enumerate discovers the tests in the configured project by asking xcodebuild for\n" +
			"them, applies tests.include/exclude, and prints what is left.\n\n" +
			"Discovery runs on one booted simulator and installs and launches the test host,\n" +
			"so it takes as long as a single app launch does.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateFormat(format, formatList, formatJSON, formatTree); err != nil {
				return err
			}
			cfg, err := global.load(true)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("xcodebuild-output") {
				cfg.Execution.XcodebuildOutput = xcodeOutput
			}

			result, err := executor.New(cfg).Enumerate(cmd.Context(), executor.EnumerateOptions{
				Device: device,
				DryRun: dryRun,
				// stderr, so the list on stdout is still pipeable. Whether
				// anything is written there is the config's call.
				Output: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				fmt.Fprintln(out, result.Command)
				return nil
			}
			warnIfEmpty(cmd.ErrOrStderr(), result)
			return printEnumeration(out, result, format, verbose)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&format, "format", "f", formatList, "output format: list, json or tree")
	f.StringVar(&device, "device", "", "simulator to enumerate on, by UDID or name (default: the first eligible one)")
	f.BoolVar(&dryRun, "dry-run", false, "print the xcodebuild command instead of running it")
	f.BoolVarP(&verbose, "verbose", "v", false, "also report filtered and disabled tests")
	f.BoolVar(&xcodeOutput, "xcodebuild-output", false, "override execution.xcodebuildOutput; stream xcodebuild's own output to stderr as it runs")
	return cmd
}

// warnIfEmpty explains an empty result, which is nearly always a filter or a
// test plan that does not select what its author expected.
func warnIfEmpty(w io.Writer, e *executor.Enumeration) {
	if e.Count() > 0 {
		return
	}
	var filtered, disabled int
	for _, plan := range e.Plans {
		filtered += len(plan.Filtered)
		disabled += len(plan.Disabled)
	}
	switch {
	case filtered > 0:
		fmt.Fprintf(w, "warning: no tests selected — tests.include/exclude dropped all %d of them\n", filtered)
	case disabled > 0:
		fmt.Fprintf(w, "warning: no tests selected — all %d are disabled by the test plan\n", disabled)
	default:
		fmt.Fprintln(w, "warning: no tests selected — xcodebuild reported none for this project")
	}
}

func printEnumeration(w io.Writer, e *executor.Enumeration, format string, verbose bool) error {
	switch format {
	case formatJSON:
		return writeJSON(w, e)

	case formatTree:
		for _, plan := range e.Plans {
			if len(e.Plans) > 1 {
				fmt.Fprintf(w, "%s\n", plan.Name)
			}
			printTree(w, executor.BuildTree(plan.Tests), "")
		}

	default:
		for _, plan := range e.Plans {
			for _, test := range plan.Tests {
				fmt.Fprintln(w, test)
			}
		}
	}

	if format != formatJSON && verbose {
		printEnumerationDetail(w, e)
	}
	return nil
}

// printEnumerationDetail writes the counts and the tests that will not run to
// stderr-style trailing output, keeping the primary list clean for piping.
func printEnumerationDetail(w io.Writer, e *executor.Enumeration) {
	fmt.Fprintf(w, "\n%d test(s) on %s\n", e.Count(), e.Device)
	for _, plan := range e.Plans {
		if len(plan.Filtered) > 0 {
			fmt.Fprintf(w, "\nfiltered out by tests.include/exclude (%d):\n", len(plan.Filtered))
			for _, test := range plan.Filtered {
				fmt.Fprintf(w, "  %s\n", test)
			}
		}
		if len(plan.Disabled) > 0 {
			fmt.Fprintf(w, "\ndisabled by the test plan (%d):\n", len(plan.Disabled))
			for _, test := range plan.Disabled {
				fmt.Fprintf(w, "  %s\n", test)
			}
		}
	}
}

// printTree renders a test tree with box-drawing connectors.
func printTree(w io.Writer, nodes []*executor.TreeNode, prefix string) {
	for i, node := range nodes {
		last := i == len(nodes)-1
		connector, childPrefix := "├── ", prefix+"│   "
		if last {
			connector, childPrefix = "└── ", prefix+"    "
		}
		fmt.Fprintf(w, "%s%s%s\n", prefix, connector, node.Name)
		printTree(w, node.Children, childPrefix)
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func validateFormat(format string, allowed ...string) error {
	for _, a := range allowed {
		if format == a {
			return nil
		}
	}
	return fmt.Errorf("unknown format %q: want %s", format, strings.Join(allowed, ", "))
}
