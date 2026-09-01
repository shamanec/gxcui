package simctl

import (
	"context"
	"strings"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

func TestBoot(t *testing.T) {
	fake := exec.NewFake(exec.Response{Stdout: "Device already booted"})

	if err := Boot(context.Background(), fake, "xcpool-1"); err != nil {
		t.Fatalf("Boot() error = %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("Boot() made %d calls, want 1", len(calls))
	}
	// -b is what makes this boot rather than only watch, and bootstatus is what
	// makes it wait for the simulator to be usable.
	if got, want := calls[0].String(), "xcrun simctl bootstatus xcpool-1 -b"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestBootFailureNamesTheDeviceAndTheReason(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 164, Stderr: "Invalid device: xcpool-9"})

	err := Boot(context.Background(), fake, "xcpool-9")
	if err == nil {
		t.Fatal("Boot() error = nil, want an error for a non-zero exit")
	}
	for _, want := range []string{"xcpool-9", "Invalid device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Boot() error = %q, want it to contain %q", err, want)
		}
	}
}

// A simulator that never comes up leaves its progress on stdout and says
// nothing on stderr, so the error has to fall back to that or say nothing useful.
func TestBootFailureFallsBackToStdout(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 1, Stdout: "Waiting to boot...\nboot failed: still preparing\n"})

	err := Boot(context.Background(), fake, "xcpool-1")
	if err == nil {
		t.Fatal("Boot() error = nil, want an error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "still preparing") {
		t.Errorf("Boot() error = %q, want it to contain the last line of stdout", err)
	}
}
