package main

import (
	"fmt"
	"io"

	"github.com/shamanec/gxcui/executor"
	"github.com/spf13/cobra"
)

func newDevicesCommand(global *globalFlags) *cobra.Command {
	var (
		format string
		all    bool
	)

	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List the simulators gxcui would use",
		Long: "Devices reports which booted simulators are eligible for this configuration.\n\n" +
			"It reads the inventory as it is now and changes nothing: simulators.bootSims\n" +
			"applies to `gxcui run`, so a simulator listed here as not booted is one that\n" +
			"a run would boot.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateFormat(format, formatList, formatJSON); err != nil {
				return err
			}
			// Listing simulators does not need a project, so an unconfigured or
			// incomplete one must not block it.
			cfg, err := global.load(false)
			if err != nil {
				return err
			}

			sel, err := executor.New(cfg).SelectDevices(cmd.Context())
			if err != nil {
				return err
			}
			return printDevices(cmd.OutOrStdout(), sel, format, all)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&format, "format", "f", formatList, "output format: list or json")
	f.BoolVarP(&all, "all", "a", false, "also show simulators that were skipped, and why")
	return cmd
}

func printDevices(w io.Writer, sel *executor.DeviceSelection, format string, all bool) error {
	if format == formatJSON {
		if !all {
			sel = &executor.DeviceSelection{Selected: sel.Selected}
		}
		return writeJSON(w, sel)
	}

	if len(sel.Selected) == 0 {
		fmt.Fprintln(w, "no eligible simulators")
	}
	for _, d := range sel.Selected {
		fmt.Fprintf(w, "%s\n", d)
	}
	if all && len(sel.Skipped) > 0 {
		fmt.Fprintf(w, "\nskipped (%d):\n", len(sel.Skipped))
		for _, s := range sel.Skipped {
			fmt.Fprintf(w, "  %s — %s\n", s.Device, s.Reason)
		}
	}
	return nil
}
