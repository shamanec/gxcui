package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
	"github.com/shamanec/gxcui/internal/simctl"
	"github.com/shamanec/gxcui/internal/xcodebuild"
)

// Executor drives test discovery and execution for one configuration.
type Executor struct {
	cfg    *Config
	runner exec.Runner
}

// New returns an Executor that runs real xcodebuild and simctl processes.
func New(cfg *Config) *Executor {
	return &Executor{cfg: cfg, runner: exec.OS{}}
}

// Config returns the configuration the Executor was built with.
func (e *Executor) Config() *Config { return e.cfg }

// Device is a simulator gxcui can run tests on.
type Device struct {
	UDID       string `json:"udid"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Platform   string `json:"platform,omitempty"`
	OSVersion  string `json:"osVersion,omitempty"`
	DeviceType string `json:"deviceType,omitempty"`
}

// Booted reports whether the simulator is running.
func (d Device) Booted() bool { return d.State == simctl.StateBooted }

// String renders the device for human-facing output.
func (d Device) String() string {
	if d.Platform == "" {
		return fmt.Sprintf("%s (%s)", d.Name, d.UDID)
	}
	return fmt.Sprintf("%s (%s) %s %s", d.Name, d.UDID, d.Platform, d.OSVersion)
}

func toDevice(d simctl.Device) Device {
	return Device{
		UDID:       d.UDID,
		Name:       d.Name,
		State:      d.State,
		Platform:   d.Platform(),
		OSVersion:  d.OSVersion(),
		DeviceType: d.DeviceType,
	}
}

// SkippedDevice is a simulator gxcui will not use, and why.
type SkippedDevice struct {
	Device Device `json:"device"`
	Reason string `json:"reason"`
}

// DeviceSelection is the outcome of applying the simulators configuration to
// the current simulator inventory.
type DeviceSelection struct {
	// Selected holds the eligible booted simulators, in a stable order.
	Selected []Device `json:"selected"`
	// Skipped holds every other simulator with the reason it was passed over.
	Skipped []SkippedDevice `json:"skipped,omitempty"`
}

// SelectDevices returns the simulators eligible for this configuration.
//
// A simulator qualifies when it is booted, is not named in simulators.exclude,
// and — if simulators.include is non-empty — is named in it. Matching accepts
// either a UDID (case-insensitive) or an exact device name.
func (e *Executor) SelectDevices(ctx context.Context) (*DeviceSelection, error) {
	devices, err := simctl.List(ctx, e.runner)
	if err != nil {
		return nil, err
	}

	include := e.cfg.Simulators.Include
	exclude := e.cfg.Simulators.Exclude

	sel := &DeviceSelection{}
	for _, d := range devices {
		device := toDevice(d)
		switch {
		case !d.Booted():
			sel.Skipped = append(sel.Skipped, SkippedDevice{device, "not booted"})
		case matchesAny(d, exclude):
			sel.Skipped = append(sel.Skipped, SkippedDevice{device, "excluded by simulators.exclude"})
		case len(include) > 0 && !matchesAny(d, include):
			sel.Skipped = append(sel.Skipped, SkippedDevice{device, "not listed in simulators.include"})
		default:
			sel.Selected = append(sel.Selected, device)
		}
	}
	return sel, nil
}

// matchesAny reports whether the device is named by any selector, which may be
// a UDID or a device name.
func matchesAny(d simctl.Device, selectors []string) bool {
	for _, s := range selectors {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.EqualFold(s, d.UDID) || s == d.Name {
			return true
		}
	}
	return false
}

// pickDevice returns the simulator to use for a single-device operation such as
// enumeration. When udid is empty the first eligible simulator is used.
func (e *Executor) pickDevice(ctx context.Context, udid string) (Device, error) {
	sel, err := e.SelectDevices(ctx)
	if err != nil {
		return Device{}, err
	}

	if udid != "" {
		for _, d := range sel.Selected {
			if strings.EqualFold(d.UDID, udid) || d.Name == udid {
				return d, nil
			}
		}
		for _, s := range sel.Skipped {
			if strings.EqualFold(s.Device.UDID, udid) || s.Device.Name == udid {
				return Device{}, fmt.Errorf("simulator %q is unusable: %s", udid, s.Reason)
			}
		}
		return Device{}, fmt.Errorf("no simulator matches %q: pass a UDID or device name from `gxcui devices`", udid)
	}

	if len(sel.Selected) == 0 {
		return Device{}, noEligibleDeviceError(sel)
	}
	return sel.Selected[0], nil
}

func noEligibleDeviceError(sel *DeviceSelection) error {
	var booted int
	for _, s := range sel.Skipped {
		if s.Device.Booted() {
			booted++
		}
	}
	if booted > 0 {
		return fmt.Errorf("no eligible simulator: %d booted simulator(s) were filtered out by simulators.include/exclude", booted)
	}
	return fmt.Errorf("no booted simulators: boot one with `xcrun simctl boot <udid>`, then retry")
}

// xcodebuildProject renders the configured project as an xcodebuild input.
func (c *Config) xcodebuildProject() xcodebuild.Project {
	return xcodebuild.Project{
		XCTestRun:       c.Project.XCTestRun,
		TestProducts:    c.Project.TestProducts,
		Project:         c.Project.Project,
		Workspace:       c.Project.Workspace,
		Scheme:          c.Project.Scheme,
		TestPlan:        c.Project.TestPlan,
		Configuration:   c.Project.Configuration,
		DerivedDataPath: c.Project.DerivedDataPath,
	}
}
