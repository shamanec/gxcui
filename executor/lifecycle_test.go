package executor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

// The UDIDs of the simulators in testdata/devices.json. xcpool-1 and xcpool-2
// are booted, the iPhone SE is not, and xcpool-3 is unavailable so it never
// reaches the inventory at all.
const (
	udidPool1    = "92F3C99D-476B-4BA5-B857-A7FAB6C60349"
	udidPool2    = "8442C46F-83D8-4A3B-8F34-47A4CE4C34D9"
	udidIPhoneSE = "AD2B8BC3-6650-403E-8194-F17A697E65DE"
)

// simRunner answers `simctl list devices` from the fixture and accepts every
// other simctl command, recording the lot.
type simRunner struct {
	t           *testing.T
	devicesJSON string

	mu       sync.Mutex
	commands []string
}

func newSimRunner(t *testing.T) *simRunner {
	return &simRunner{t: t, devicesJSON: fixture(t, "devices.json")}
}

func (r *simRunner) Run(_ context.Context, cmd exec.Command) (*exec.Result, error) {
	r.mu.Lock()
	r.commands = append(r.commands, cmd.String())
	r.mu.Unlock()

	args := strings.Join(cmd.Args, " ")
	if cmd.Name != "xcrun" || !strings.HasPrefix(args, "simctl ") {
		r.t.Fatalf("unexpected command: %s", cmd)
	}
	if strings.Contains(args, "list devices") {
		return &exec.Result{Stdout: r.devicesJSON}, nil
	}
	return &exec.Result{}, nil
}

func (r *simRunner) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

// index returns where a command containing substr was run, or -1.
func (r *simRunner) index(substr string) int {
	for i, cmd := range r.ran() {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

func (r *simRunner) ranCommand(t *testing.T, substr string) {
	t.Helper()
	if r.index(substr) < 0 {
		t.Errorf("no command matching %q was run; ran:\n  %s", substr, strings.Join(r.ran(), "\n  "))
	}
}

func (r *simRunner) didNotRun(t *testing.T, substr string) {
	t.Helper()
	if i := r.index(substr); i >= 0 {
		t.Errorf("ran %q, which should not have happened", r.ran()[i])
	}
}

func lifecycleConfig(mutate func(*Config)) *Config {
	cfg := DefaultConfig()
	mutate(&cfg)
	return &cfg
}

// A reset is scoped to the simulators the run owns, and stops at erasing them:
// bringing a simulator back up is simulators.bootSims' job, and one gxcui did
// not boot is not gxcui's to restore.
func TestResetSimulatorsErasesTheIncludeListAndLeavesItDown(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) {
		c.Simulators.Include = []string{"xcpool-1", "xcpool-2"}
		c.Simulators.BootSims = true
		c.Simulators.ResetBefore = true
	})
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.resetSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("resetSimulators() error = %v", err)
	}

	for _, udid := range []string{udidPool1, udidPool2} {
		shutdown := runner.index("simctl shutdown " + udid)
		erase := runner.index("simctl erase " + udid)
		switch {
		case shutdown < 0 || erase < 0:
			t.Errorf("%s: shutdown/erase = %d/%d, want both to have run", udid, shutdown, erase)
		case shutdown > erase:
			// simctl refuses to erase a booted device.
			t.Errorf("%s was erased before it was shut down", udid)
		}
	}

	runner.didNotRun(t, "bootstatus")
	// A simulator outside the include list is somebody else's.
	runner.didNotRun(t, udidIPhoneSE)
	runner.didNotRun(t, " all")
}

func TestResetSimulatorsIsSkippedWhenOff(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) { c.Simulators.Include = []string{"xcpool-1"} })
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.resetSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("resetSimulators() error = %v", err)
	}
	if ran := runner.ran(); len(ran) != 0 {
		t.Errorf("resetSimulators() ran %v, want nothing when simulators.resetBefore is off", ran)
	}
}

func TestShutdownSimulatorsShutsDownOnlyTheScopedOnes(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) {
		c.Simulators.Include = []string{"xcpool-1"}
		c.Simulators.ShutdownAfter = true
	})
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.shutdownSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("shutdownSimulators() error = %v", err)
	}

	runner.ranCommand(t, "simctl shutdown "+udidPool1)
	runner.didNotRun(t, udidPool2)
	// Shutting down is not erasing: the run is over, and what it left behind is
	// the next run's business.
	runner.didNotRun(t, "simctl erase")
}

// With no include list the run is "every booted simulator", so the shutdown
// covers the whole machine — which simctl does in one command.
func TestShutdownSimulatorsWithoutIncludeShutsDownEverything(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) { c.Simulators.ShutdownAfter = true })
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.shutdownSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("shutdownSimulators() error = %v", err)
	}
	runner.ranCommand(t, "simctl shutdown all")
}

// "all" cannot express an exception, so an exclude list turns the shutdown back
// into one command per simulator. An excluded simulator is one gxcui was told
// to keep its hands off.
func TestShutdownSimulatorsWithoutIncludeStillHonoursExclude(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) {
		c.Simulators.Exclude = []string{"xcpool-2"}
		c.Simulators.ShutdownAfter = true
	})
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.shutdownSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("shutdownSimulators() error = %v", err)
	}

	runner.didNotRun(t, " all")
	runner.didNotRun(t, udidPool2)
	runner.ranCommand(t, "simctl shutdown "+udidPool1)
	// The iPhone SE is in scope but already down, so there is nothing to do to
	// it: a shutdown per idle simulator would be noise.
	runner.didNotRun(t, udidIPhoneSE)
}

// The context that stopped the batches is already cancelled by the time the
// cleanup runs, so an interrupted run would otherwise leave its simulators up.
func TestShutdownSimulatorsRunsAfterAnInterruptedRun(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) { c.Simulators.ShutdownAfter = true })
	e := &Executor{cfg: cfg, runner: runner}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := e.shutdownSimulators(ctx, RunOptions{}); err != nil {
		t.Fatalf("shutdownSimulators() error = %v", err)
	}
	runner.ranCommand(t, "simctl shutdown all")
}

func TestShutdownSimulatorsIsSkippedWhenOff(t *testing.T) {
	runner := newSimRunner(t)
	cfg := lifecycleConfig(func(c *Config) { c.Simulators.Include = []string{"xcpool-1"} })
	e := &Executor{cfg: cfg, runner: runner}

	if err := e.shutdownSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("shutdownSimulators() error = %v", err)
	}
	if ran := runner.ran(); len(ran) != 0 {
		t.Errorf("shutdownSimulators() ran %v, want nothing when simulators.shutdownAfter is off", ran)
	}
}

// The reset has to be the first thing a run does — the simulators are erased,
// then booted, and only then is the pool read — and the shutdown the last, once
// no batch needs a device any more.
func TestRunResetsFirstAndShutsDownLast(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()"})

	cfg := runConfig(t)
	cfg.Simulators.BootSims = true
	cfg.Simulators.ResetBefore = true
	cfg.Simulators.ShutdownAfter = true
	e := &Executor{cfg: &cfg, runner: fake}

	if _, err := e.Run(context.Background(), RunOptions{Now: fixedClock()}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	erase := indexOf(fake.commands, "simctl erase")
	boot := indexOf(fake.commands, "bootstatus")
	test := indexOf(fake.commands, "-only-testing:")
	shutdownAfter := lastIndexOf(fake.commands, "simctl shutdown")
	switch {
	case erase < 0 || boot < 0 || shutdownAfter < 0:
		t.Fatalf("erase/boot/shutdown = %d/%d/%d, want the run to have done all three:\n  %s",
			erase, boot, shutdownAfter, strings.Join(fake.commands, "\n  "))
	case erase > boot:
		t.Errorf("the simulators were booted before they were erased, so the run would start on a dirty device")
	case boot > test:
		t.Errorf("the tests started before the simulators were booted")
	case shutdownAfter < test:
		t.Errorf("the simulators were shut down before the tests finished")
	}
}

func lastIndexOf(commands []string, substr string) int {
	for i := len(commands) - 1; i >= 0; i-- {
		if strings.Contains(commands[i], substr) {
			return i
		}
	}
	return -1
}
