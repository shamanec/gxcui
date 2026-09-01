// Package simctl reads the simulator inventory via `xcrun simctl`, and boots,
// shuts down and erases simulators when asked to.
//
// Everything here beyond reading the inventory changes a simulator, so nothing
// here happens unless the configuration turns it on: those are the operations
// that can destroy a simulator someone else was using, and recovering from that
// is tedious. gxcui never creates or deletes one.
package simctl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// StateBooted is the simctl state string for a running simulator.
const StateBooted = "Booted"

// Device is one simulator from the inventory.
type Device struct {
	UDID        string `json:"udid"`
	Name        string `json:"name"`
	State       string `json:"state"`
	IsAvailable bool   `json:"isAvailable"`
	DeviceType  string `json:"deviceTypeIdentifier"`

	// Runtime is the raw runtime identifier the device was listed under, e.g.
	// "com.apple.CoreSimulator.SimRuntime.iOS-26-5". simctl reports it as the
	// key of the devices map rather than as a device field.
	Runtime string `json:"-"`
}

// Booted reports whether the device is currently running.
func (d Device) Booted() bool { return d.State == StateBooted }

// Platform returns the OS family of the runtime, e.g. "iOS". It returns an
// empty string for a runtime identifier in an unexpected shape.
func (d Device) Platform() string {
	name, _, ok := splitRuntime(d.Runtime)
	if !ok {
		return ""
	}
	return name
}

// OSVersion returns the runtime version in dotted form, e.g. "26.5". It returns
// an empty string for a runtime identifier in an unexpected shape.
func (d Device) OSVersion() string {
	_, version, ok := splitRuntime(d.Runtime)
	if !ok {
		return ""
	}
	return version
}

// String renders the device the way gxcui reports it to users.
func (d Device) String() string {
	platform, version := d.Platform(), d.OSVersion()
	if platform == "" {
		return fmt.Sprintf("%s (%s)", d.Name, d.UDID)
	}
	return fmt.Sprintf("%s (%s) %s %s", d.Name, d.UDID, platform, version)
}

// splitRuntime turns "com.apple.CoreSimulator.SimRuntime.iOS-26-5" into
// ("iOS", "26.5"). The final path component is <name>-<major>-<minor>[-patch],
// where the name itself never contains a dash.
func splitRuntime(runtime string) (name, version string, ok bool) {
	idx := strings.LastIndex(runtime, ".")
	if idx >= 0 {
		runtime = runtime[idx+1:]
	}
	name, rest, found := strings.Cut(runtime, "-")
	if !found || name == "" || rest == "" {
		return "", "", false
	}
	return name, strings.ReplaceAll(rest, "-", "."), true
}

type deviceList struct {
	Devices map[string][]Device `json:"devices"`
}

// List returns every simulator known to simctl, sorted by runtime then name.
// Devices marked unavailable (a missing runtime, most often) are dropped: they
// can never host a test run.
func List(ctx context.Context, r exec.Runner) ([]Device, error) {
	res, err := r.Run(ctx, exec.Command{Name: "xcrun", Args: []string{"simctl", "list", "devices", "-j"}})
	if err != nil {
		return nil, fmt.Errorf("simctl list devices: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("simctl list devices: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var parsed deviceList
	if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
		return nil, fmt.Errorf("simctl list devices: parse output: %w", err)
	}

	var devices []Device
	for runtime, inRuntime := range parsed.Devices {
		for _, d := range inRuntime {
			if !d.IsAvailable {
				continue
			}
			d.Runtime = runtime
			devices = append(devices, d)
		}
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Runtime != devices[j].Runtime {
			return devices[i].Runtime < devices[j].Runtime
		}
		if devices[i].Name != devices[j].Name {
			return devices[i].Name < devices[j].Name
		}
		return devices[i].UDID < devices[j].UDID
	})
	return devices, nil
}

// Booted returns the subset of List that is currently running.
func Booted(ctx context.Context, r exec.Runner) ([]Device, error) {
	all, err := List(ctx, r)
	if err != nil {
		return nil, err
	}
	booted := make([]Device, 0, len(all))
	for _, d := range all {
		if d.Booted() {
			booted = append(booted, d)
		}
	}
	return booted, nil
}
