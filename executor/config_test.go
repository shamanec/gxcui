package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shamanec/gxcui/reporter"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gxcui.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
project:
  workspace: App.xcworkspace
  scheme: AppUITests
  testPlan: Smoke
simulators:
  include: [xcpool-1, xcpool-2]
  exclude: [xcpool-3]
  bootSims: true
  bootTimeout: 2m
tests:
  exclude:
    - "App/FlakyTests/*"
  enumerate:
    timeout: 90s
output:
  html:
    coverage: true
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if got, want := cfg.Project.Scheme, "AppUITests"; got != want {
		t.Errorf("Scheme = %q, want %q", got, want)
	}
	if got, want := cfg.Tests.Enumerate.Timeout.Duration(), 90*time.Second; got != want {
		t.Errorf("enumerate timeout = %v, want %v", got, want)
	}
	if got, want := len(cfg.Simulators.Include), 2; got != want {
		t.Errorf("len(simulators.include) = %d, want %d", got, want)
	}
	if !cfg.Simulators.BootSims {
		t.Error("simulators.bootSims = false, want true")
	}
	if got, want := cfg.Simulators.BootTimeout.Duration(), 2*time.Minute; got != want {
		t.Errorf("simulators.bootTimeout = %v, want %v", got, want)
	}
	if !cfg.Output.HTML.Coverage {
		t.Error("output.html.coverage = false, want true")
	}
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	path := writeConfig(t, "project:\n  xctestrun: App.xctestrun\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Version != ConfigVersion {
		t.Errorf("Version = %d, want %d", cfg.Version, ConfigVersion)
	}
	if cfg.Tests.Enumerate.Timeout <= 0 {
		t.Error("enumerate timeout should have a default")
	}
	// Touching simulators is opt-in: a config that says nothing about booting
	// must leave their lifecycle alone.
	if cfg.Simulators.BootSims {
		t.Error("simulators.bootSims defaults to on, want off")
	}
	if cfg.Simulators.BootTimeout <= 0 {
		t.Error("simulators.bootTimeout should have a default")
	}
	// Coverage costs a pass over every bundle and is absent from most schemes,
	// so an existing config must not start paying for it.
	if cfg.Output.HTML.Coverage {
		t.Error("output.html.coverage defaults to on, want off")
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	// A misspelled key must fail rather than silently do nothing.
	path := writeConfig(t, "project:\n  xctestrun: App.xctestrun\n  schemee: App\n")

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "schemee") {
		t.Errorf("LoadConfig() error = %q, want it to name the unknown key", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "no input",
			mutate:  func(c *Config) {},
			wantErr: "no test input configured",
		},
		{
			name: "two inputs",
			mutate: func(c *Config) {
				c.Project.Workspace = "App.xcworkspace"
				c.Project.XCTestRun = "App.xctestrun"
			},
			wantErr: "conflicting test inputs",
		},
		{
			name:    "workspace without scheme",
			mutate:  func(c *Config) { c.Project.Workspace = "App.xcworkspace" },
			wantErr: "project.scheme is required",
		},
		{
			name: "scheme with xctestrun",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Project.Scheme = "App"
			},
			wantErr: "project.scheme cannot be combined",
		},
		{
			name: "test plan with xctestrun",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Project.TestPlan = "Smoke"
			},
			wantErr: "project.testPlan cannot be combined",
		},
		{
			name: "bad pattern",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Tests.Exclude = []string{"re:["}
			},
			wantErr: "tests.exclude",
		},
		{
			name: "bootSims without include",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Simulators.BootSims = true
			},
			wantErr: "simulators.bootSims needs simulators.include",
		},
		{
			name: "bootSims with everything excluded",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Simulators.BootSims = true
				c.Simulators.Include = []string{"xcpool-1"}
				c.Simulators.Exclude = []string{"xcpool-1"}
			},
			wantErr: "simulators.bootSims needs simulators.include",
		},
		{
			name: "unsupported version",
			mutate: func(c *Config) {
				c.Project.XCTestRun = "App.xctestrun"
				c.Version = 99
			},
			wantErr: "unsupported config version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateWithoutProject(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.ValidateWithoutProject(); err != nil {
		t.Errorf("ValidateWithoutProject() error = %v, want nil for an unconfigured project", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() error = nil, want an error for an unconfigured project")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		yaml string
		want time.Duration
	}{
		{"timeout: 5m", 5 * time.Minute},
		{"timeout: 90s", 90 * time.Second},
		{`timeout: "1h30m"`, 90 * time.Minute},
		{"timeout: 45", 45 * time.Second},
	}

	for _, tt := range tests {
		path := writeConfig(t, "project:\n  xctestrun: App.xctestrun\ntests:\n  enumerate:\n    "+tt.yaml+"\n")
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%q) error = %v", tt.yaml, err)
		}
		if got := cfg.Tests.Enumerate.Timeout.Duration(); got != tt.want {
			t.Errorf("%q -> %v, want %v", tt.yaml, got, tt.want)
		}
	}
}

func TestDurationUnmarshalRejectsGarbage(t *testing.T) {
	path := writeConfig(t, "project:\n  xctestrun: App.xctestrun\ntests:\n  enumerate:\n    timeout: soon\n")

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig() error = nil, want an error for an invalid duration")
	}
}

func TestFindConfig(t *testing.T) {
	dir := t.TempDir()
	if got := FindConfig(dir); got != "" {
		t.Errorf("FindConfig() = %q, want empty for a directory with no config", got)
	}

	path := filepath.Join(dir, "gxcui.yml")
	if err := os.WriteFile(path, []byte("project:\n  xctestrun: App.xctestrun\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if got := FindConfig(dir); got != path {
		t.Errorf("FindConfig() = %q, want %q", got, path)
	}
}

// The example config is documentation people copy, and strict decoding means a
// key that has been renamed or removed makes it fail outright rather than be
// quietly ignored.
func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "gxcui.example.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig(gxcui.example.yaml) error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	// Its values should be the documented defaults, not something the example
	// alone turns on.
	if !cfg.Output.HTML.Enabled.Enabled() {
		t.Error("the example turns the HTML report off")
	}
	if cfg.Output.HTML.Activities != reporter.DetailFailed {
		t.Errorf("example html.activities = %q, want the documented default %q",
			cfg.Output.HTML.Activities, reporter.DetailFailed)
	}
}
