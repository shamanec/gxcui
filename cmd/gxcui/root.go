package main

import (
	"fmt"

	"github.com/shamanec/gxcui/executor"
	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// globalFlags are the settings every command shares. Each one, when set,
// overrides the corresponding value from the config file.
type globalFlags struct {
	configPath string

	workspace       string
	project         string
	xctestrun       string
	testProducts    string
	scheme          string
	testPlan        string
	configuration   string
	derivedDataPath string

	include []string
	exclude []string

	simulators []string
}

func newRootCommand() *cobra.Command {
	var flags globalFlags

	root := &cobra.Command{
		Use:   "gxcui",
		Short: "Run XCUITests in parallel across booted simulators",
		Long: "gxcui enumerates the tests in an Xcode project, splits them into batches and runs\n" +
			"each batch on a different booted simulator.\n\n" +
			"Configuration comes from gxcui.yaml in the current directory, from --config, or\n" +
			"entirely from flags. Flags always win over the config file.",
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       version,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&flags.configPath, "config", "c", "", "path to gxcui.yaml (default: gxcui.yaml in the current directory, if present)")
	pf.StringVar(&flags.workspace, "workspace", "", "path to an .xcworkspace")
	pf.StringVar(&flags.project, "project", "", "path to an .xcodeproj")
	pf.StringVar(&flags.xctestrun, "xctestrun", "", "path to a prebuilt .xctestrun file")
	pf.StringVar(&flags.testProducts, "test-products", "", "path to a prebuilt .xctestproducts archive")
	pf.StringVar(&flags.scheme, "scheme", "", "scheme to test (required with --workspace or --project)")
	pf.StringVar(&flags.testPlan, "test-plan", "", "test plan name, without the .xctestplan extension")
	pf.StringVar(&flags.configuration, "configuration", "", "build configuration, e.g. Debug")
	pf.StringVar(&flags.derivedDataPath, "derived-data-path", "", "derived data directory")
	pf.StringArrayVar(&flags.include, "include", nil, "keep only tests matching this pattern (repeatable)")
	pf.StringArrayVar(&flags.exclude, "exclude", nil, "drop tests matching this pattern (repeatable)")
	pf.StringArrayVar(&flags.simulators, "simulator", nil, "restrict to this simulator, by UDID or name (repeatable)")

	root.AddCommand(
		newDevicesCommand(&flags),
		newEnumerateCommand(&flags),
		newRunCommand(&flags),
		newReportCommand(),
	)
	return root
}

// load resolves the effective configuration: the config file if there is one,
// with flag overrides applied on top, validated.
//
// requireProject is false for commands that do not need something to test, so
// that `gxcui devices` works in a directory with no project configured.
func (f *globalFlags) load(requireProject bool) (*executor.Config, error) {
	cfg := executor.DefaultConfig()

	path := f.configPath
	if path == "" {
		path = executor.FindConfig(".")
	}
	if path != "" {
		loaded, err := executor.LoadConfig(path)
		if err != nil {
			return nil, err
		}
		cfg = *loaded
	}

	f.applyTo(&cfg)

	err := cfg.ValidateWithoutProject()
	if err == nil && requireProject {
		err = cfg.Project.Validate()
	}
	if err != nil {
		if cfg.Path() != "" {
			return nil, fmt.Errorf("%s: %w", cfg.Path(), err)
		}
		return nil, err
	}
	return &cfg, nil
}

// applyTo overlays the flags that were given onto cfg. A test input flag clears
// the other three, so that --xctestrun on the command line replaces a workspace
// from the config file instead of conflicting with it.
func (f *globalFlags) applyTo(cfg *executor.Config) {
	switch {
	case f.workspace != "":
		cfg.Project.Workspace, cfg.Project.Project, cfg.Project.XCTestRun, cfg.Project.TestProducts = f.workspace, "", "", ""
	case f.project != "":
		cfg.Project.Workspace, cfg.Project.Project, cfg.Project.XCTestRun, cfg.Project.TestProducts = "", f.project, "", ""
	case f.xctestrun != "":
		cfg.Project.Workspace, cfg.Project.Project, cfg.Project.XCTestRun, cfg.Project.TestProducts = "", "", f.xctestrun, ""
		cfg.Project.Scheme, cfg.Project.TestPlan = "", ""
	case f.testProducts != "":
		cfg.Project.Workspace, cfg.Project.Project, cfg.Project.XCTestRun, cfg.Project.TestProducts = "", "", "", f.testProducts
		cfg.Project.Scheme, cfg.Project.TestPlan = "", ""
	}

	setString(&cfg.Project.Scheme, f.scheme)
	setString(&cfg.Project.TestPlan, f.testPlan)
	setString(&cfg.Project.Configuration, f.configuration)
	setString(&cfg.Project.DerivedDataPath, f.derivedDataPath)

	if len(f.include) > 0 {
		cfg.Tests.Include = f.include
	}
	if len(f.exclude) > 0 {
		cfg.Tests.Exclude = f.exclude
	}
	if len(f.simulators) > 0 {
		cfg.Simulators.Include = f.simulators
	}
}

func setString(dst *string, value string) {
	if value != "" {
		*dst = value
	}
}
