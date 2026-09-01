// Package executor discovers tests, splits them into batches and runs those
// batches across booted simulators.
//
// The package is usable as a library: build a Config by hand, or load one from
// YAML with LoadConfig, and hand it to the entry points in this package. Nothing
// here reads command-line flags or writes to stdout.
package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shamanec/gxcui/reporter"
	"gopkg.in/yaml.v3"
)

// ConfigVersion is the schema version this build understands.
const ConfigVersion = 1

// DefaultConfigNames are the file names looked up when no config path is given.
var DefaultConfigNames = []string{"gxcui.yaml", "gxcui.yml"}

// Config is the whole of gxcui's configuration.
type Config struct {
	// Version pins the config schema. Omitted means the current version.
	Version int `yaml:"version"`

	Project    ProjectConfig    `yaml:"project"`
	Simulators SimulatorsConfig `yaml:"simulators"`
	Tests      TestsConfig      `yaml:"tests"`
	Batching   BatchingConfig   `yaml:"batching"`
	Execution  ExecutionConfig  `yaml:"execution"`
	Retries    RetriesConfig    `yaml:"retries"`
	Output     OutputConfig     `yaml:"output"`

	// path records where the config was loaded from, for error messages.
	path string `yaml:"-"`
}

// ProjectConfig selects what to test. Exactly one of Workspace, Project,
// XCTestRun or TestProducts must be set.
type ProjectConfig struct {
	// Workspace is a path to an .xcworkspace.
	Workspace string `yaml:"workspace"`
	// Project is a path to an .xcodeproj.
	Project string `yaml:"project"`
	// XCTestRun is a path to a prebuilt .xctestrun file. This is the fastest
	// input: no build step, and safe to share across parallel runs.
	XCTestRun string `yaml:"xctestrun"`
	// TestProducts is a path to a prebuilt .xctestproducts archive.
	TestProducts string `yaml:"testProducts"`

	// Scheme is required with Workspace or Project.
	Scheme string `yaml:"scheme"`
	// TestPlan is a test plan name, without the .xctestplan extension.
	TestPlan string `yaml:"testPlan"`
	// Configuration is the build configuration, e.g. "Debug".
	Configuration string `yaml:"configuration"`
	// DerivedDataPath isolates build products and logs from other Xcode users.
	DerivedDataPath string `yaml:"derivedDataPath"`
}

// SimulatorsConfig selects which simulators gxcui may use, and whether it boots
// them itself.
//
// gxcui never erases, creates or shuts down a simulator. Booting is the one
// exception, it is off by default, and it only ever touches the simulators
// Include names.
type SimulatorsConfig struct {
	// Include lists UDIDs or device names to use. Empty means every booted
	// simulator is eligible.
	Include []string `yaml:"include"`
	// Exclude lists UDIDs or device names to skip. It wins over Include.
	Exclude []string `yaml:"exclude"`
	// BootSims boots the simulators named in Include before the run starts,
	// instead of expecting them to be booted already. It needs Include to be
	// set: gxcui will not go looking for simulators to boot.
	BootSims bool `yaml:"bootSims"`
	// BootTimeout bounds one simulator's boot. A simulator that is not up by
	// then fails the run rather than holding it up indefinitely.
	BootTimeout Duration `yaml:"bootTimeout"`
}

// TestsConfig narrows the enumerated test set.
type TestsConfig struct {
	// Include keeps only matching tests. Empty means keep everything.
	Include []string `yaml:"include"`
	// Exclude drops matching tests. It is applied after Include.
	Exclude []string `yaml:"exclude"`
	// Enumerate tunes the discovery step.
	Enumerate EnumerateConfig `yaml:"enumerate"`
}

// EnumerateConfig tunes test discovery.
type EnumerateConfig struct {
	// Timeout bounds a single enumeration run. Enumeration installs and launches
	// the test host, so a real UI test target needs considerably longer than the
	// few seconds a trivial one takes.
	Timeout Duration `yaml:"timeout"`
}

// BatchingConfig controls how tests are split across simulators.
type BatchingConfig struct {
	// Strategy is one of duration, class, count or shard.
	Strategy Strategy `yaml:"strategy"`
	// Batches is how many batches to produce. Zero means twice the number of
	// simulators, so a simulator that finishes early can take more work rather
	// than idle while another finishes a long batch.
	Batches int `yaml:"batches"`
	// BatchSize is the number of tests per batch, used only by the count
	// strategy.
	BatchSize int `yaml:"batchSize"`
}

// ExecutionConfig controls how each batch is run.
type ExecutionConfig struct {
	// BatchTimeout bounds a single batch. A batch that overruns is killed and
	// its tests are treated as unaccounted for, so they can be retried.
	BatchTimeout Duration `yaml:"batchTimeout"`
	// TestTimeout, in seconds, caps any individual test. Zero leaves the test
	// plan's own setting alone.
	TestTimeout int `yaml:"testTimeout"`
	// ExtraArgs are appended verbatim to every xcodebuild test invocation.
	ExtraArgs []string `yaml:"extraArgs"`
	// BuildArgs are appended verbatim to build-for-testing, for things like
	// code signing settings.
	BuildArgs []string `yaml:"buildArgs"`
}

// RetriesConfig controls what happens to tests that fail.
type RetriesConfig struct {
	// MaxAttempts is how many times a test may run in total. One means no
	// retries.
	MaxAttempts int `yaml:"maxAttempts"`
	// Isolate re-runs each failed test in a batch of its own, so a test that
	// only fails alongside a particular neighbour gets a fair second chance.
	Isolate Toggle `yaml:"isolate"`
}

// OutputConfig controls what a run leaves behind.
type OutputConfig struct {
	// Dir is where per-run directories are created.
	Dir string `yaml:"dir"`
	// Merge combines the per-batch result bundles into one, which is what
	// xcresult consumers such as an HTML reporter expect.
	Merge Toggle `yaml:"merge"`
	// JUnit writes a JUnit XML report.
	JUnit Toggle `yaml:"junit"`
	// HTML writes a self-contained HTML report from the merged bundle.
	HTML HTMLConfig `yaml:"html"`
	// KeepResultBundles retains the per-batch bundles after merging.
	KeepResultBundles Toggle `yaml:"keepResultBundles"`
	// TimingsFile records per-test durations for future batching. Empty
	// disables the feature.
	TimingsFile string `yaml:"timingsFile"`
}

// HTMLConfig controls the HTML report.
//
// The report is one file with its screenshots and recordings inlined, so it can
// be archived by CI or attached to a bug as-is. That also means the two detail
// settings decide how big it gets: a full UI test run with every screenshot
// embedded can reach hundreds of megabytes.
type HTMLConfig struct {
	// Enabled turns the report on. It defaults to on.
	Enabled Toggle `yaml:"enabled"`
	// Path is where to write it, relative to the run directory unless absolute.
	Path string `yaml:"path"`
	// Activities selects which tests include their step-by-step log: none,
	// failed or all.
	Activities reporter.Detail `yaml:"activities"`
	// Attachments selects which tests embed their screenshots and recordings:
	// none, failed or all.
	Attachments reporter.Detail `yaml:"attachments"`
	// MaxAttachmentSizeMB drops any single attachment larger than this, so one
	// long screen recording cannot make the report unopenable. Zero means no
	// limit.
	MaxAttachmentSizeMB int `yaml:"maxAttachmentSizeMB"`
	// Coverage adds a line-coverage breakdown per target and source file.
	//
	// It defaults to off. Coverage only exists when the scheme was set to
	// gather it, and reading it costs an extra pass over every batch bundle,
	// so it is opt-in rather than something every run pays for.
	Coverage bool `yaml:"coverage"`
}

// DefaultConfig returns a Config with every default applied.
func DefaultConfig() Config {
	var c Config
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = ConfigVersion
	}
	if c.Simulators.BootTimeout == 0 {
		c.Simulators.BootTimeout = Duration(5 * time.Minute)
	}
	if c.Tests.Enumerate.Timeout == 0 {
		c.Tests.Enumerate.Timeout = Duration(10 * time.Minute)
	}
	if c.Batching.Strategy == "" {
		c.Batching.Strategy = StrategyDuration
	}
	if c.Execution.BatchTimeout == 0 {
		c.Execution.BatchTimeout = Duration(30 * time.Minute)
	}
	if c.Retries.MaxAttempts == 0 {
		c.Retries.MaxAttempts = 1
	}
	if c.Output.Dir == "" {
		c.Output.Dir = filepath.Join(".gxcui", "runs")
	}
	if c.Output.TimingsFile == "" {
		c.Output.TimingsFile = filepath.Join(".gxcui", "timings.json")
	}
	if c.Output.HTML.Path == "" {
		c.Output.HTML.Path = "report.html"
	}
	// Collecting activities and screenshots for tests that passed costs an
	// xcresulttool call each and can add gigabytes to the report, for material
	// nobody looks at. Failures are where both actually earn their keep.
	if c.Output.HTML.Activities == "" {
		c.Output.HTML.Activities = reporter.DetailFailed
	}
	if c.Output.HTML.Attachments == "" {
		c.Output.HTML.Attachments = reporter.DetailFailed
	}
}

// Validate reports whether the configuration describes a runnable set-up.
func (c *Config) Validate() error {
	if err := c.ValidateWithoutProject(); err != nil {
		return err
	}
	return c.Project.Validate()
}

// ValidateWithoutProject checks everything except the project inputs. It is for
// operations that do not need something to test, such as listing simulators.
func (c *Config) ValidateWithoutProject() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("unsupported config version %d: this build understands version %d", c.Version, ConfigVersion)
	}
	if c.Simulators.BootTimeout <= 0 {
		return fmt.Errorf("simulators.bootTimeout must be positive")
	}
	if c.Simulators.BootSims && len(c.bootTargets()) == 0 {
		return fmt.Errorf("simulators.bootSims needs simulators.include: gxcui only boots the simulators you name")
	}
	if err := validatePatterns("tests.include", c.Tests.Include); err != nil {
		return err
	}
	if err := validatePatterns("tests.exclude", c.Tests.Exclude); err != nil {
		return err
	}
	if c.Tests.Enumerate.Timeout <= 0 {
		return fmt.Errorf("tests.enumerate.timeout must be positive")
	}
	if !c.Batching.Strategy.Valid() {
		return fmt.Errorf("batching.strategy %q is not one of %s", c.Batching.Strategy, strategyNames())
	}
	if c.Batching.Batches < 0 {
		return fmt.Errorf("batching.batches cannot be negative")
	}
	if c.Batching.BatchSize < 0 {
		return fmt.Errorf("batching.batchSize cannot be negative")
	}
	if c.Execution.BatchTimeout <= 0 {
		return fmt.Errorf("execution.batchTimeout must be positive")
	}
	if c.Execution.TestTimeout < 0 {
		return fmt.Errorf("execution.testTimeout cannot be negative")
	}
	if c.Retries.MaxAttempts < 1 {
		return fmt.Errorf("retries.maxAttempts must be at least 1")
	}
	if c.Output.Dir == "" {
		return fmt.Errorf("output.dir cannot be empty")
	}
	if !c.Output.HTML.Activities.Valid() {
		return fmt.Errorf("output.html.activities %q is not %s", c.Output.HTML.Activities, reporter.DetailNames())
	}
	if !c.Output.HTML.Attachments.Valid() {
		return fmt.Errorf("output.html.attachments %q is not %s", c.Output.HTML.Attachments, reporter.DetailNames())
	}
	if c.Output.HTML.MaxAttachmentSizeMB < 0 {
		return fmt.Errorf("output.html.maxAttachmentSizeMB cannot be negative")
	}
	return nil
}

// Validate reports whether exactly one usable test input is configured.
func (p ProjectConfig) Validate() error {
	inputs := map[string]string{
		"project.workspace":    p.Workspace,
		"project.project":      p.Project,
		"project.xctestrun":    p.XCTestRun,
		"project.testProducts": p.TestProducts,
	}
	var set []string
	for name, value := range inputs {
		if value != "" {
			set = append(set, name)
		}
	}
	switch len(set) {
	case 0:
		return fmt.Errorf("no test input configured: set one of project.workspace, project.project, project.xctestrun or project.testProducts")
	case 1:
	default:
		sort.Strings(set)
		return fmt.Errorf("conflicting test inputs (%s): set exactly one", strings.Join(set, ", "))
	}

	needsScheme := p.Workspace != "" || p.Project != ""
	switch {
	case needsScheme && p.Scheme == "":
		return fmt.Errorf("project.scheme is required with %s", set[0])
	case !needsScheme && p.Scheme != "":
		return fmt.Errorf("project.scheme cannot be combined with %s: the prebuilt input already carries the scheme's settings", set[0])
	case !needsScheme && p.TestPlan != "":
		return fmt.Errorf("project.testPlan cannot be combined with %s: it was built for one specific test plan", set[0])
	}
	return nil
}

// LoadConfig reads a YAML config from path, applies defaults and validates it.
//
// Decoding is strict: an unknown or misspelled key is an error rather than a
// setting that silently does nothing.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	defer f.Close()

	cfg := Config{path: path}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	cfg.applyDefaults()
	return &cfg, nil
}

// FindConfig returns the path of the first DefaultConfigNames entry present in
// dir, or "" when there is none.
func FindConfig(dir string) string {
	for _, name := range DefaultConfigNames {
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	return ""
}

// Path reports where the config was loaded from, or "" for one built in memory.
func (c *Config) Path() string { return c.path }

// Duration is a time.Duration that unmarshals from a YAML string such as "5m".
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the value the way it is written in YAML.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts either a duration string ("90s", "5m") or a plain
// number of seconds.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("invalid duration: want a string like \"5m\" or a number of seconds")
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}
	// A bare number is read as seconds. YAML hands scalars over as strings, so
	// this is the same code path as "5m" rather than a separate decode.
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	return fmt.Errorf("invalid duration %q: want a string like \"5m\" or a number of seconds", s)
}

// MarshalYAML writes the value as a duration string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Toggle is a boolean setting that is on unless the configuration turns it off.
//
// A plain bool cannot express this: its zero value is false, so an omitted key
// and an explicit "false" look identical, and the default would quietly
// override the user. Toggle records whether the value was set at all.
type Toggle struct {
	set   bool
	value bool
}

// On returns a Toggle explicitly turned on.
func On() Toggle { return Toggle{set: true, value: true} }

// Off returns a Toggle explicitly turned off.
func Off() Toggle { return Toggle{set: true, value: false} }

// Enabled reports whether the setting is active, defaulting to true.
func (t Toggle) Enabled() bool { return !t.set || t.value }

// String renders the effective value.
func (t Toggle) String() string { return strconv.FormatBool(t.Enabled()) }

// UnmarshalYAML reads a boolean and records that it was given.
func (t *Toggle) UnmarshalYAML(value *yaml.Node) error {
	var v bool
	if err := value.Decode(&v); err != nil {
		return fmt.Errorf("invalid boolean %q", value.Value)
	}
	*t = Toggle{set: true, value: v}
	return nil
}

// MarshalYAML writes the effective value.
func (t Toggle) MarshalYAML() (any, error) { return t.Enabled(), nil }
