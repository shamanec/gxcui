// Package exec is the seam between gxcui and the external tools it drives
// (xcodebuild, xcrun, simctl). Everything that shells out goes through Runner so
// that the rest of the codebase can be tested without Xcode installed.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
)

// Command describes a single external process invocation.
type Command struct {
	// Name is the executable to run, e.g. "xcodebuild".
	Name string
	// Args are passed to the executable verbatim, without shell interpretation.
	Args []string
	// Dir is the working directory. Empty means the current process' directory.
	Dir string
	// Env holds extra KEY=VALUE entries appended to the parent environment.
	Env []string
	// Stdout, when non-nil, receives a copy of the process' standard output as
	// it is produced. Result.Stdout is populated regardless.
	Stdout io.Writer
	// Stderr behaves like Stdout for standard error.
	Stderr io.Writer
}

// String renders the command the way a user would type it. It is meant for logs
// and --dry-run output, not for re-execution through a shell.
func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, c.Name)
	for _, a := range c.Args {
		if strings.ContainsAny(a, " \t\"'") {
			parts = append(parts, fmt.Sprintf("%q", a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// Result is the outcome of a finished process.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes commands. Run returns a Result for any process that started
// and exited, including one that exited non-zero; a non-nil error means the
// process could not be started, was cancelled, or failed for a reason unrelated
// to its own exit status. Callers that care about exit codes must therefore
// inspect Result rather than relying on err.
type Runner interface {
	Run(ctx context.Context, cmd Command) (*Result, error)
}

// OS is a Runner backed by real processes.
type OS struct{}

var _ Runner = OS{}

// Run starts cmd in its own process group and waits for it to exit. When ctx is
// cancelled the whole group is killed, which matters because xcodebuild spawns
// children (test runners, simulator helpers) that outlive a signal sent only to
// the parent.
func (OS) Run(ctx context.Context, cmd Command) (*Result, error) {
	var stdout, stderr bytes.Buffer

	c := osexec.Command(cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	c.Stdout = &stdout
	c.Stderr = &stderr
	if cmd.Stdout != nil {
		c.Stdout = io.MultiWriter(&stdout, cmd.Stdout)
	}
	if cmd.Stderr != nil {
		c.Stderr = io.MultiWriter(&stderr, cmd.Stderr)
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Name, err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	select {
	case <-ctx.Done():
		killGroup(c.Process.Pid)
		<-done
		return nil, ctx.Err()
	case err := <-done:
		res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
		var exitErr *osexec.ExitError
		switch {
		case err == nil:
			return res, nil
		case errors.As(err, &exitErr):
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		default:
			return res, fmt.Errorf("run %s: %w", cmd.Name, err)
		}
	}
}

func killGroup(pid int) {
	// Negative pid targets the process group created via Setpgid.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
