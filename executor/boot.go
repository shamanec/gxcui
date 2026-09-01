package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/simctl"
)

// bootTargets returns the simulators a run would boot: every simulators.include
// entry that simulators.exclude does not cancel out, deduplicated, in the order
// they were configured.
//
// It returns nothing when booting is off, and nothing when include is empty —
// "every booted simulator" names no simulator to boot.
func (c *Config) bootTargets() []string {
	if !c.Simulators.BootSims {
		return nil
	}

	seen := map[string]bool{}
	targets := make([]string, 0, len(c.Simulators.Include))
	for _, name := range c.Simulators.Include {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if name == "" || seen[key] || excludes(c.Simulators.Exclude, name) {
			continue
		}
		seen[key] = true
		targets = append(targets, name)
	}
	return targets
}

// bootList renders the boot targets for an error message.
func (c *Config) bootList() string { return strings.Join(c.bootTargets(), ", ") }

// excludes reports whether an exclude entry cancels out an include entry.
// Booting one and then refusing to run on it would be pure waste, and the
// selection step drops it either way.
func excludes(exclude []string, name string) bool {
	for _, e := range exclude {
		if strings.EqualFold(strings.TrimSpace(e), name) {
			return true
		}
	}
	return false
}

// bootSimulators boots every configured simulator and waits for all of them.
//
// A simulator that fails to boot fails the run. It was named in the config, so
// running without it would quietly mean less parallelism than was asked for —
// or, when it was the only one, no run at all a step later.
func (e *Executor) bootSimulators(ctx context.Context, opts RunOptions) error {
	targets := e.cfg.bootTargets()
	if len(targets) == 0 {
		return nil
	}

	opts.emit(Event{Type: EventBootStarted, Total: len(targets),
		Message: fmt.Sprintf("%d simulator(s): %s", len(targets), strings.Join(targets, ", "))})

	return e.eachSimulator(ctx, targets, simOp{
		verb:        "boot",
		timeout:     e.cfg.Simulators.BootTimeout.Duration(),
		timeoutHint: "simulators.bootTimeout",
		run: func(ctx context.Context, target string) error {
			return simctl.Boot(ctx, e.runner, target)
		},
		done: func(target string, completed, total int) {
			opts.emit(Event{Type: EventBootFinished, Message: target,
				Completed: completed, Total: total})
		},
	})
}
