package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/shamanec/gxcui/executor"
)

// progressPrinter renders run events for a terminal.
//
// On a TTY it keeps a status line pinned to the bottom showing what each
// simulator is doing, and prints finished batches above it. Anywhere else — a
// pipe, a CI log — it prints plain sequential lines with no cursor tricks, since
// escape codes in a log file help nobody.
type progressPrinter struct {
	out io.Writer
	tty bool

	mu      sync.Mutex
	current map[string]string // device UDID -> batch id
	devices []executor.Device
	drawn   bool
}

func newProgressPrinter(out io.Writer, tty bool) *progressPrinter {
	return &progressPrinter{out: out, tty: tty, current: map[string]string{}}
}

// ANSI sequences used for the pinned status line.
const (
	ansiClearLine = "\r\033[2K"
	ansiHideCur   = "\033[?25l"
	ansiShowCur   = "\033[?25h"
)

func (p *progressPrinter) handle(e executor.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch e.Type {
	case executor.EventResetStarted:
		p.line("Erasing %s", e.Message)
	case executor.EventResetFinished:
		p.line("Erased %s", e.Message)
	case executor.EventShutdownStarted:
		p.line("Shutting down %s", e.Message)
	case executor.EventShutdownFinished:
		p.line("Shut down %s", e.Message)
	case executor.EventBootStarted:
		p.line("Booting %s", e.Message)
	case executor.EventBootFinished:
		p.line("Booted %s (%d/%d)", e.Message, e.Completed, e.Total)
	case executor.EventBuildStarted:
		p.line("Building for testing…")
	case executor.EventBuildFinished:
		p.line("Built %s", e.Message)
	case executor.EventEnumerated:
		p.line("Found %s", e.Message)
	case executor.EventPlanned:
		p.line("Planned %s", e.Message)
		p.devices = nil
	case executor.EventRetryStarted:
		p.line("Attempt %d: %s", e.Attempt, e.Message)

	case executor.EventBatchStarted:
		p.track(e.Device)
		p.current[e.Device.UDID] = e.BatchID
		p.status(e.Completed, e.Total)

	case executor.EventBatchFinished:
		delete(p.current, e.Device.UDID)
		p.line("%s", batchSummary(e))
		p.status(e.Completed, e.Total)

	case executor.EventReporting:
		p.clear()
		p.line("Writing reports…")
	}
}

// finish removes the status line once the run is over.
func (p *progressPrinter) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
}

func (p *progressPrinter) track(d executor.Device) {
	for _, known := range p.devices {
		if known.UDID == d.UDID {
			return
		}
	}
	p.devices = append(p.devices, d)
}

// line prints a completed message above the status line.
func (p *progressPrinter) line(format string, args ...any) {
	if p.tty {
		fmt.Fprint(p.out, ansiClearLine)
		p.drawn = false
	}
	fmt.Fprintf(p.out, format+"\n", args...)
}

// status redraws the pinned progress line. It is a no-op off a TTY.
func (p *progressPrinter) status(completed, total int) {
	if !p.tty {
		return
	}

	var running []string
	for udid, batch := range p.current {
		running = append(running, deviceName(p.devices, udid)+" → "+batch)
	}
	sort.Strings(running)

	text := fmt.Sprintf("%d/%d batches", completed, total)
	if len(running) > 0 {
		text += " │ " + strings.Join(running, " │ ")
	}

	fmt.Fprint(p.out, ansiClearLine, ansiHideCur, text)
	p.drawn = true
}

func (p *progressPrinter) clear() {
	if p.tty && p.drawn {
		fmt.Fprint(p.out, ansiClearLine, ansiShowCur)
		p.drawn = false
	}
}

func deviceName(devices []executor.Device, udid string) string {
	for _, d := range devices {
		if d.UDID == udid {
			return d.Name
		}
	}
	if len(udid) > 8 {
		return udid[:8]
	}
	return udid
}

// batchSummary renders a one-line verdict for a finished batch.
func batchSummary(e executor.Event) string {
	b := e.Batch
	if b == nil {
		return e.BatchID + " finished"
	}

	verdict := fmt.Sprintf("%d passed", b.Passed)
	if b.Failed > 0 {
		verdict += fmt.Sprintf(", %d failed", b.Failed)
	}
	if b.Skipped > 0 {
		verdict += fmt.Sprintf(", %d skipped", b.Skipped)
	}
	if n := len(b.Unaccounted); n > 0 {
		verdict += fmt.Sprintf(", %d unaccounted", n)
	}

	mark := "✓"
	if b.Failed > 0 || b.Status != executor.BatchCompleted {
		mark = "✗"
	}
	status := ""
	if b.Status != executor.BatchCompleted {
		status = " [" + string(b.Status) + "]"
	}

	return fmt.Sprintf("%s %s on %s — %s in %.0fs%s",
		mark, b.ID, b.Device.Name, verdict, b.Seconds, status)
}

// isTTY reports whether w is an interactive terminal.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
