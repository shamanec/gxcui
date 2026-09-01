package simctl

import (
	"context"
	"strings"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

func TestShutdown(t *testing.T) {
	fake := exec.NewFake(exec.Response{})

	if err := Shutdown(context.Background(), fake, "xcpool-1"); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("Shutdown() made %d calls, want 1", len(calls))
	}
	if got, want := calls[0].String(), "xcrun simctl shutdown xcpool-1"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

// simctl refuses to shut down a device that is already down. The caller asked
// for a shut-down simulator and has one, so that is not a failure.
func TestShutdownToleratesAnAlreadyShutDownDevice(t *testing.T) {
	fake := exec.NewFake(exec.Response{
		ExitCode: 149,
		Stderr:   "Unable to shutdown device in current state: Shutdown",
	})

	if err := Shutdown(context.Background(), fake, "xcpool-1"); err != nil {
		t.Errorf("Shutdown() error = %v, want nil for a device that was already down", err)
	}
}

func TestShutdownFailureNamesTheDeviceAndTheReason(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 164, Stderr: "Invalid device: xcpool-9"})

	err := Shutdown(context.Background(), fake, "xcpool-9")
	if err == nil {
		t.Fatal("Shutdown() error = nil, want an error for a non-zero exit")
	}
	for _, want := range []string{"xcpool-9", "Invalid device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Shutdown() error = %q, want it to contain %q", err, want)
		}
	}
}

func TestErase(t *testing.T) {
	fake := exec.NewFake(exec.Response{})

	if err := Erase(context.Background(), fake, AllDevices); err != nil {
		t.Fatalf("Erase() error = %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("Erase() made %d calls, want 1", len(calls))
	}
	if got, want := calls[0].String(), "xcrun simctl erase all"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

// Erasing a booted device is the mistake this reports, and the error has to say
// so rather than leaving a caller to guess why a clean start did not happen.
func TestEraseFailureNamesTheReason(t *testing.T) {
	fake := exec.NewFake(exec.Response{
		ExitCode: 149,
		Stderr:   "Unable to erase contents and settings in current state: Booted",
	})

	err := Erase(context.Background(), fake, "xcpool-1")
	if err == nil {
		t.Fatal("Erase() error = nil, want an error for a non-zero exit")
	}
	for _, want := range []string{"xcpool-1", "current state: Booted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Erase() error = %q, want it to contain %q", err, want)
		}
	}
}
