package simctl

import (
	"context"
	"os"
	"testing"

	"github.com/shamanec/gxcui/internal/exec"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

func TestList(t *testing.T) {
	fake := exec.NewFake(exec.Response{Stdout: fixture(t, "devices.json")})

	devices, err := List(context.Background(), fake)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	// xcpool-3 is unavailable and must be dropped.
	if len(devices) != 3 {
		t.Fatalf("List() returned %d devices, want 3: %v", len(devices), devices)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("List() made %d calls, want 1", len(calls))
	}
	if got, want := calls[0].String(), "xcrun simctl list devices -j"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}

	// Sorted by runtime, then name: iOS-18-1 comes before iOS-26-5.
	if got, want := devices[0].Name, "iPhone SE (3rd generation)"; got != want {
		t.Errorf("devices[0].Name = %q, want %q", got, want)
	}
	if got, want := devices[1].Name, "xcpool-1"; got != want {
		t.Errorf("devices[1].Name = %q, want %q", got, want)
	}

	booted := devices[1]
	if !booted.Booted() {
		t.Errorf("%s should be booted", booted.Name)
	}
	if got, want := booted.Platform(), "iOS"; got != want {
		t.Errorf("Platform() = %q, want %q", got, want)
	}
	if got, want := booted.OSVersion(), "26.5"; got != want {
		t.Errorf("OSVersion() = %q, want %q", got, want)
	}
	if got, want := booted.String(), "xcpool-1 (92F3C99D-476B-4BA5-B857-A7FAB6C60349) iOS 26.5"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestBooted(t *testing.T) {
	fake := exec.NewFake(exec.Response{Stdout: fixture(t, "devices.json")})

	devices, err := Booted(context.Background(), fake)
	if err != nil {
		t.Fatalf("Booted() error = %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("Booted() returned %d devices, want 2: %v", len(devices), devices)
	}
	for _, d := range devices {
		if !d.Booted() {
			t.Errorf("%s is not booted", d.Name)
		}
	}
}

func TestListNonZeroExit(t *testing.T) {
	fake := exec.NewFake(exec.Response{ExitCode: 1, Stderr: "simctl exploded"})

	if _, err := List(context.Background(), fake); err == nil {
		t.Fatal("List() error = nil, want an error for a non-zero exit")
	}
}

func TestSplitRuntime(t *testing.T) {
	tests := []struct {
		runtime string
		name    string
		version string
		ok      bool
	}{
		{"com.apple.CoreSimulator.SimRuntime.iOS-26-5", "iOS", "26.5", true},
		{"com.apple.CoreSimulator.SimRuntime.iOS-17-0-1", "iOS", "17.0.1", true},
		{"com.apple.CoreSimulator.SimRuntime.watchOS-11-2", "watchOS", "11.2", true},
		{"nonsense", "", "", false},
		{"", "", "", false},
	}

	for _, tt := range tests {
		name, version, ok := splitRuntime(tt.runtime)
		if name != tt.name || version != tt.version || ok != tt.ok {
			t.Errorf("splitRuntime(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.runtime, name, version, ok, tt.name, tt.version, tt.ok)
		}
	}
}
