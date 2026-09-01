package simctl

import (
	"context"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// AllDevices is simctl's keyword for every simulator on the machine. It is
// accepted anywhere a device is, and doing the whole machine in one command is
// far cheaper than one command per simulator when there are dozens of them.
const AllDevices = "all"

// Shutdown shuts a simulator down. device is a UDID, a device name, or
// AllDevices.
//
// A device that is already shut down is not an error. simctl refuses the
// transition, but the caller asked for a shut-down simulator and has one.
func Shutdown(ctx context.Context, r exec.Runner, device string) error {
	res, err := r.Run(ctx, exec.Command{Name: "xcrun", Args: []string{"simctl", "shutdown", device}})
	if err != nil {
		return fmt.Errorf("simctl shutdown %s: %w", device, err)
	}
	if res.ExitCode != 0 && !alreadyShutdown(res) {
		return fmt.Errorf("simctl shutdown %s: exit %d: %s", device, res.ExitCode, failureReason(res))
	}
	return nil
}

// Erase erases a simulator's contents and settings, leaving it as it was when
// it was created: no installed apps, no granted permissions, no keychain, none
// of the state the last run left behind.
//
// The device must be shut down first — simctl refuses to erase a booted one —
// so callers pair this with Shutdown rather than calling it on its own.
func Erase(ctx context.Context, r exec.Runner, device string) error {
	res, err := r.Run(ctx, exec.Command{Name: "xcrun", Args: []string{"simctl", "erase", device}})
	if err != nil {
		return fmt.Errorf("simctl erase %s: %w", device, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("simctl erase %s: exit %d: %s", device, res.ExitCode, failureReason(res))
	}
	return nil
}

// alreadyShutdown reports whether a failed shutdown failed only because the
// device was down to begin with. simctl phrases that as a refused state
// transition, the same way it refuses to boot a booted device.
func alreadyShutdown(res *exec.Result) bool {
	return strings.Contains(res.Stderr, "current state: Shutdown")
}
