package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shamanec/gxcui/internal/exec"
)

func TestBootTargets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   []string
	}{
		{
			name:   "off by default",
			mutate: func(c *Config) { c.Simulators.Include = []string{"xcpool-1"} },
			want:   nil,
		},
		{
			name: "the include list, in order",
			mutate: func(c *Config) {
				c.Simulators.BootSims = true
				c.Simulators.Include = []string{"xcpool-2", "xcpool-1"}
			},
			want: []string{"xcpool-2", "xcpool-1"},
		},
		{
			name: "exclude wins",
			mutate: func(c *Config) {
				c.Simulators.BootSims = true
				c.Simulators.Include = []string{"xcpool-1", "xcpool-2"}
				c.Simulators.Exclude = []string{"xcpool-2"}
			},
			want: []string{"xcpool-1"},
		},
		{
			name: "deduplicated and trimmed",
			mutate: func(c *Config) {
				c.Simulators.BootSims = true
				c.Simulators.Include = []string{"xcpool-1", " xcpool-1 ", "", "XCPOOL-1"}
			},
			want: []string{"xcpool-1"},
		},
		{
			name:   "nothing to boot without an include list",
			mutate: func(c *Config) { c.Simulators.BootSims = true },
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)

			got := cfg.bootTargets()
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("bootTargets() = %v, want %v", got, tt.want)
			}
		})
	}
}

// bootRunner answers bootstatus calls, and holds each one until every expected
// boot has arrived. A sequential implementation would never reach the barrier,
// so this fails rather than passing slowly.
type bootRunner struct {
	t       *testing.T
	want    int
	release chan struct{}

	mu       sync.Mutex
	booted   []string
	arrived  int
	failures map[string]string
}

func newBootRunner(t *testing.T, want int) *bootRunner {
	return &bootRunner{t: t, want: want, release: make(chan struct{}), failures: map[string]string{}}
}

func (b *bootRunner) Run(ctx context.Context, cmd exec.Command) (*exec.Result, error) {
	if len(cmd.Args) < 2 || cmd.Args[1] != "bootstatus" {
		b.t.Fatalf("unexpected command: %s", cmd)
	}
	device := cmd.Args[2]

	b.mu.Lock()
	b.booted = append(b.booted, device)
	b.arrived++
	if b.arrived == b.want {
		close(b.release)
	}
	reason, fails := b.failures[device]
	b.mu.Unlock()

	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		b.t.Errorf("boot of %s was not concurrent with the others", device)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if fails {
		return &exec.Result{ExitCode: 1, Stderr: reason}, nil
	}
	return &exec.Result{}, nil
}

func (b *bootRunner) devices() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.booted...)
}

func bootConfig(targets ...string) *Config {
	cfg := DefaultConfig()
	cfg.Project.XCTestRun = "App.xctestrun"
	cfg.Simulators.BootSims = true
	cfg.Simulators.Include = targets
	return &cfg
}

func TestBootSimulatorsBootsInParallel(t *testing.T) {
	targets := []string{"xcpool-1", "xcpool-2", "xcpool-3"}
	runner := newBootRunner(t, len(targets))
	e := &Executor{cfg: bootConfig(targets...), runner: runner}

	var events []Event
	var mu sync.Mutex
	opts := RunOptions{Progress: func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}}

	if err := e.bootSimulators(context.Background(), opts); err != nil {
		t.Fatalf("bootSimulators() error = %v", err)
	}

	if got := runner.devices(); len(got) != len(targets) {
		t.Fatalf("booted %v, want %v", got, targets)
	}

	var started, finished int
	for _, ev := range events {
		switch ev.Type {
		case EventBootStarted:
			started++
			if !strings.Contains(ev.Message, "xcpool-1") {
				t.Errorf("boot-started message = %q, want it to name the simulators", ev.Message)
			}
		case EventBootFinished:
			finished++
		}
	}
	if started != 1 || finished != len(targets) {
		t.Errorf("got %d boot-started and %d boot-finished events, want 1 and %d", started, finished, len(targets))
	}
}

func TestBootSimulatorsReportsEveryFailure(t *testing.T) {
	targets := []string{"xcpool-1", "xcpool-2", "xcpool-3"}
	runner := newBootRunner(t, len(targets))
	runner.failures["xcpool-1"] = "Invalid device"
	runner.failures["xcpool-3"] = "Unable to boot device in current state: Shutting Down"

	e := &Executor{cfg: bootConfig(targets...), runner: runner}

	err := e.bootSimulators(context.Background(), RunOptions{})
	if err == nil {
		t.Fatal("bootSimulators() error = nil, want an error for a simulator that would not boot")
	}
	// One bad simulator must not hide another: the run is about to be short of
	// both of them.
	for _, want := range []string{"xcpool-1", "xcpool-3", "Invalid device", "Shutting Down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bootSimulators() error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "xcpool-2") {
		t.Errorf("bootSimulators() error = %q, want it to leave out the simulator that booted", err)
	}
}

func TestBootSimulatorsTimesOut(t *testing.T) {
	cfg := bootConfig("xcpool-1")
	cfg.Simulators.BootTimeout = Duration(50 * time.Millisecond)

	// A simulator that never finishes booting: the runner returns only when its
	// context is cancelled, which is what the real runner does on a deadline.
	stuck := runnerFunc(func(ctx context.Context, _ exec.Command) (*exec.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	e := &Executor{cfg: cfg, runner: stuck}

	err := e.bootSimulators(context.Background(), RunOptions{})
	if err == nil {
		t.Fatal("bootSimulators() error = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "bootTimeout") {
		t.Errorf("bootSimulators() error = %q, want it to name simulators.bootTimeout", err)
	}
}

func TestBootSimulatorsIsSkippedWhenOff(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Simulators.Include = []string{"xcpool-1"}

	refuse := runnerFunc(func(context.Context, exec.Command) (*exec.Result, error) {
		return nil, fmt.Errorf("nothing should have been run")
	})
	e := &Executor{cfg: &cfg, runner: refuse}

	if err := e.bootSimulators(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("bootSimulators() error = %v, want nil when simulators.bootSims is off", err)
	}
}

// Booting has to happen before anything looks at the simulator inventory: a
// simulator that is still coming up does not count as booted, and the run would
// give up before its own boot finished.
func TestRunBootsBeforeItSelectsDevices(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()"})

	cfg := runConfig(t)
	cfg.Simulators.BootSims = true
	e := &Executor{cfg: &cfg, runner: fake}

	if _, err := e.Run(context.Background(), RunOptions{Now: fixedClock()}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var booted []string
	for i, cmd := range fake.commands {
		if !strings.Contains(cmd, "bootstatus") {
			continue
		}
		if list := indexOf(fake.commands, "simctl list devices"); list >= 0 && i > list {
			t.Errorf("%q ran after the simulator inventory was read", cmd)
		}
		booted = append(booted, cmd)
	}
	if len(booted) != 2 {
		t.Fatalf("ran %d bootstatus commands, want one per configured simulator: %v", len(booted), booted)
	}
	for i, want := range []string{"xcpool-1", "xcpool-2"} {
		if !strings.Contains(strings.Join(booted, "\n"), want) {
			t.Errorf("boot commands %v do not cover %s (%d)", booted, want, i)
		}
	}
}

// A dry run plans the boot and prints it, but must not perform it.
func TestDryRunReportsButDoesNotBoot(t *testing.T) {
	fake := newFakeXcode(t, []string{"App/AlphaTests/testOne()"})

	cfg := runConfig(t)
	cfg.Simulators.BootSims = true
	e := &Executor{cfg: &cfg, runner: fake}

	plan, err := e.DryRun(context.Background(), RunOptions{Now: fixedClock()})
	if err != nil {
		t.Fatalf("DryRun() error = %v", err)
	}
	if got, want := strings.Join(plan.Boot, ","), "xcpool-1,xcpool-2"; got != want {
		t.Errorf("plan.Boot = %q, want %q", got, want)
	}
	for _, cmd := range fake.commands {
		if strings.Contains(cmd, "bootstatus") {
			t.Errorf("dry run booted a simulator: %s", cmd)
		}
	}
}

func indexOf(commands []string, substr string) int {
	for i, cmd := range commands {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

type runnerFunc func(context.Context, exec.Command) (*exec.Result, error)

func (f runnerFunc) Run(ctx context.Context, cmd exec.Command) (*exec.Result, error) {
	return f(ctx, cmd)
}
