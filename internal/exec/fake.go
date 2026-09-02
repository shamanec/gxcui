package exec

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Response is a canned outcome for a single Fake invocation.
type Response struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
	// Do, when set, runs before the response is returned. It is the hook for
	// commands with side effects, such as xcodebuild writing its test
	// enumeration to -test-enumeration-output-path.
	Do func(cmd Command) error
}

// Fake is a Runner that replays scripted responses in order and records every
// command it was asked to run. It is safe for concurrent use.
type Fake struct {
	mu        sync.Mutex
	responses []Response
	calls     []Command
	// Default is returned once the scripted responses are exhausted. When nil,
	// an exhausted Fake returns an error, which keeps unexpected extra
	// invocations from passing silently.
	Default *Response
}

var _ Runner = (*Fake)(nil)

// NewFake returns a Fake that replays responses in the given order.
func NewFake(responses ...Response) *Fake {
	return &Fake{responses: responses}
}

// Run records cmd and returns the next scripted response.
func (f *Fake) Run(ctx context.Context, cmd Command) (*Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, cmd)

	var resp Response
	switch {
	case len(f.responses) > 0:
		resp, f.responses = f.responses[0], f.responses[1:]
	case f.Default != nil:
		resp = *f.Default
	default:
		return nil, fmt.Errorf("exec.Fake: no scripted response for call %d: %s", len(f.calls), cmd)
	}

	if resp.Do != nil {
		if err := resp.Do(cmd); err != nil {
			return nil, err
		}
	}
	if resp.Err != nil {
		return nil, resp.Err
	}
	// Mirror OS: a caller that asked for streaming output gets it here too, so
	// the streaming paths are testable without Xcode.
	if cmd.Stdout != nil && resp.Stdout != "" {
		io.WriteString(cmd.Stdout, resp.Stdout)
	}
	if cmd.Stderr != nil && resp.Stderr != "" {
		io.WriteString(cmd.Stderr, resp.Stderr)
	}
	return &Result{Stdout: resp.Stdout, Stderr: resp.Stderr, ExitCode: resp.ExitCode}, nil
}

// Calls returns every command Run was called with, in order.
func (f *Fake) Calls() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Command(nil), f.calls...)
}
