package simctl

import (
	"context"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Boot boots a simulator and waits until it has finished booting.
//
// device is a UDID or a device name; simctl accepts either. The command is
// `simctl bootstatus <device> -b` rather than `simctl boot` because boot returns
// as soon as the boot has been started, long before the simulator can host a
// test — and a test launched at a half-booted simulator fails in ways that look
// like the test's own fault. bootstatus blocks until the device reports itself
// booted, and is safe to call on one that already is, so this is also how an
// already-running simulator is confirmed rather than disturbed.
//
// Cancelling ctx stops the wait and kills bootstatus; it does not shut the
// simulator down, which may well finish booting afterwards.
func Boot(ctx context.Context, r exec.Runner, device string) error {
	res, err := r.Run(ctx, exec.Command{Name: "xcrun", Args: []string{"simctl", "bootstatus", device, "-b"}})
	if err != nil {
		return fmt.Errorf("simctl bootstatus %s: %w", device, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("simctl bootstatus %s: exit %d: %s", device, res.ExitCode, failureReason(res))
	}
	return nil
}

// failureReason picks the part of a failed simctl command worth putting in an
// error. simctl reports the reason on stderr, but a device that never came up
// leaves its progress on stdout instead, so fall back to that rather than to
// nothing.
func failureReason(res *exec.Result) string {
	if msg := strings.TrimSpace(res.Stderr); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(res.Stdout); msg != "" {
		return lastLine(msg)
	}
	return "no output"
}

func lastLine(s string) string {
	if idx := strings.LastIndex(s, "\n"); idx >= 0 {
		return strings.TrimSpace(s[idx+1:])
	}
	return s
}
