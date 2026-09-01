package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shamanec/gxcui/internal/simctl"
)

// lifecycleTimeout bounds a single shutdown or erase. Neither takes more than a
// few seconds on a healthy machine, so this is only here to stop a simulator
// wedged in "Shutting Down" from holding the run up indefinitely.
const lifecycleTimeout = 2 * time.Minute

// lifecycleScope is the set of simulators a reset or a shutdown may touch.
//
// simulators.include names the simulators a run owns, so that is what gets
// erased or shut down. With no include list the run is "every booted
// simulator", and the matching scope is every simulator on the machine.
// simulators.exclude is honoured either way: an excluded simulator is one gxcui
// was told to keep its hands off.
type lifecycleScope struct {
	// All reports that nothing narrows the scope, so simctl can be pointed at
	// the whole machine in one command instead of one per simulator.
	All bool
	// Devices are the in-scope simulators from the inventory. With All set it
	// is still every simulator, and is what says which ones were booted.
	Devices []Device
}

// empty reports whether there is nothing to act on.
func (s lifecycleScope) empty() bool { return len(s.Devices) == 0 }

// targets returns the device arguments to hand simctl.
func (s lifecycleScope) targets() []string {
	if s.All {
		return []string{simctl.AllDevices}
	}
	return udids(s.Devices)
}

// bootedTargets is targets narrowed to the simulators that are running, for
// operations with nothing to do to a simulator that is already down.
func (s lifecycleScope) bootedTargets() []string {
	booted := s.booted()
	if len(booted) == 0 {
		return nil
	}
	if s.All {
		return []string{simctl.AllDevices}
	}
	return udids(booted)
}

// booted returns the in-scope simulators that are currently running.
func (s lifecycleScope) booted() []Device {
	var booted []Device
	for _, d := range s.Devices {
		if d.Booted() {
			booted = append(booted, d)
		}
	}
	return booted
}

func udids(devices []Device) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.UDID)
	}
	return out
}

// deviceNames renders simulators for a message. Messages name the simulators
// one by one even when simctl was pointed at "all": which simulators "all"
// covers is exactly what someone checking a destructive setting wants to see.
func deviceNames(devices []Device) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.Name)
	}
	return out
}

// lifecycleScope resolves the simulators a reset or shutdown applies to.
//
// Everything is resolved against the inventory rather than passed to simctl as
// written, so that a name in simulators.include picks out one known device and
// the reset knows which simulators it has to bring back up.
func (e *Executor) lifecycleScope(ctx context.Context) (lifecycleScope, error) {
	devices, err := simctl.List(ctx, e.runner)
	if err != nil {
		return lifecycleScope{}, err
	}

	include, exclude := e.cfg.Simulators.Include, e.cfg.Simulators.Exclude
	scope := lifecycleScope{All: len(include) == 0 && len(exclude) == 0}
	for _, d := range devices {
		if matchesAny(d, exclude) {
			continue
		}
		if len(include) > 0 && !matchesAny(d, include) {
			continue
		}
		scope.Devices = append(scope.Devices, toDevice(d))
	}
	return scope, nil
}

// resetSimulators shuts the in-scope simulators down and erases them.
//
// Erasing is what "clean" means for a simulator — no installed app, no granted
// permissions, no keychain, nothing the last run wrote — and it is the only way
// a suite starts from the same place every time. simctl refuses to erase a
// booted device, hence the shutdown first.
//
// It leaves them shut down. Booting is simulators.bootSims' job, which is why
// the configuration insists on the two together: gxcui erases the simulators it
// is about to boot, and does not decide on its own to boot anything else.
func (e *Executor) resetSimulators(ctx context.Context, opts RunOptions) error {
	if !e.cfg.Simulators.ResetBefore {
		return nil
	}

	scope, err := e.lifecycleScope(ctx)
	if err != nil {
		return err
	}
	if scope.empty() {
		return nil
	}

	names := deviceNames(scope.Devices)
	opts.emit(Event{Type: EventResetStarted, Total: len(names),
		Message: fmt.Sprintf("%d simulator(s): %s", len(names), strings.Join(names, ", "))})

	err = e.eachSimulator(ctx, scope.targets(), simOp{
		verb:    "reset",
		timeout: lifecycleTimeout,
		run: func(ctx context.Context, target string) error {
			if err := simctl.Shutdown(ctx, e.runner, target); err != nil {
				return err
			}
			return simctl.Erase(ctx, e.runner, target)
		},
	})
	if err != nil {
		return err
	}

	opts.emit(Event{Type: EventResetFinished, Total: len(names), Completed: len(names),
		Message: fmt.Sprintf("%d simulator(s)", len(names))})
	return nil
}

// shutdownSimulators shuts the in-scope simulators down once a run is over.
func (e *Executor) shutdownSimulators(ctx context.Context, opts RunOptions) error {
	if !e.cfg.Simulators.ShutdownAfter {
		return nil
	}

	// A run interrupted with Ctrl-C still releases its simulators: the
	// cancelled context that stopped the batches would otherwise kill the
	// cleanup before it had run a single command. Each command keeps its own
	// deadline, so this cannot hang either.
	ctx = context.WithoutCancel(ctx)

	scope, err := e.lifecycleScope(ctx)
	if err != nil {
		return err
	}
	targets := scope.bootedTargets()
	if len(targets) == 0 {
		return nil
	}

	names := deviceNames(scope.booted())
	opts.emit(Event{Type: EventShutdownStarted, Total: len(names),
		Message: fmt.Sprintf("%d simulator(s): %s", len(names), strings.Join(names, ", "))})

	err = e.eachSimulator(ctx, targets, simOp{
		verb:    "shutdown",
		timeout: lifecycleTimeout,
		run: func(ctx context.Context, target string) error {
			return simctl.Shutdown(ctx, e.runner, target)
		},
	})
	if err != nil {
		return err
	}

	opts.emit(Event{Type: EventShutdownFinished, Total: len(names), Completed: len(names),
		Message: fmt.Sprintf("%d simulator(s)", len(names))})
	return nil
}

// simOp is one lifecycle operation applied to a set of simulators.
type simOp struct {
	// verb names the operation in error messages, e.g. "boot".
	verb string
	// timeout bounds the operation on one simulator.
	timeout time.Duration
	// timeoutHint, when set, names the setting the bound came from so that the
	// error says what to change.
	timeoutHint string
	// run performs the operation on one simulator.
	run func(ctx context.Context, target string) error
	// done, when set, is called after each simulator finishes. It may be called
	// from several goroutines at once.
	done func(target string, completed, total int)
}

// deadline renders the bound for an error, naming the setting behind it when
// there is one.
func (op simOp) deadline() string {
	if op.timeoutHint == "" {
		return op.timeout.String()
	}
	return fmt.Sprintf("%s (%s)", op.timeoutHint, op.timeout)
}

// eachSimulator applies op to every target at once and waits for all of them.
//
// They run together because simulator operations are dominated by waiting —
// a cold boot takes tens of seconds — and doing four in sequence would put
// minutes on the front of every run. Each target gets its own deadline, so one
// wedged simulator cannot hold the run up past it, and every failure is
// reported rather than only the first: they are all simulators the run was
// counting on.
func (e *Executor) eachSimulator(ctx context.Context, targets []string, op simOp) error {
	if len(targets) == 0 {
		return nil
	}

	var (
		mu        sync.Mutex
		completed int
	)
	errs := make([]error, len(targets))

	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()

			opCtx, cancel := context.WithTimeout(ctx, op.timeout)
			defer cancel()

			switch err := op.run(opCtx, target); {
			case err == nil:
			case ctx.Err() != nil:
				errs[i] = fmt.Errorf("%s %s: %w", op.verb, target, ctx.Err())
			case errors.Is(opCtx.Err(), context.DeadlineExceeded):
				errs[i] = fmt.Errorf("%s %s: did not complete within %s", op.verb, target, op.deadline())
			default:
				errs[i] = err
			}
			if errs[i] != nil {
				return
			}

			mu.Lock()
			completed++
			done := completed
			mu.Unlock()

			if op.done != nil {
				op.done(target, done, len(targets))
			}
		}(i, target)
	}
	wg.Wait()

	return errors.Join(errs...)
}
