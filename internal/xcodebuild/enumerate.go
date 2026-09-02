package xcodebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Style selects how xcodebuild groups the tests it enumerates.
type Style string

const (
	// StyleFlat produces the identifiers that -only-testing consumes, plus the
	// set of tests the test plan disables. This is what gxcui runs on.
	StyleFlat Style = "flat"
	// StyleHierarchical groups tests by plan, target and class. gxcui derives
	// its tree view from flat identifiers instead, so this exists for callers
	// that want xcodebuild's own grouping.
	StyleHierarchical Style = "hierarchical"
)

// EnumerateOptions describes a single enumeration run.
type EnumerateOptions struct {
	Project Project
	// Destination is a full destination specifier. xcodebuild refuses to
	// enumerate without one, because enumeration actually exercises the device.
	Destination string
	// Style defaults to StyleFlat.
	Style Style
	// ExtraArgs are appended verbatim, as an escape hatch for flags gxcui does
	// not model.
	ExtraArgs []string
	// Stdout and Stderr, when set, receive the process output as it arrives.
	// The enumeration JSON is unaffected: it goes to a file, not to stdout.
	Stdout io.Writer
	Stderr io.Writer
}

// EnumerateArgs renders the full argument vector for an enumeration run writing
// its JSON to outputPath.
//
// The output always goes to a file rather than to stdout ("-"): on stdout the
// JSON is interleaved with xcodebuild's banner and its "** TEST EXECUTE
// SUCCEEDED **" trailer, which makes parsing needlessly ambiguous.
func EnumerateArgs(opts EnumerateOptions, outputPath string) ([]string, error) {
	if err := opts.Project.Validate(); err != nil {
		return nil, err
	}
	if opts.Destination == "" {
		return nil, fmt.Errorf("destination is required to enumerate tests")
	}
	style := opts.Style
	if style == "" {
		style = StyleFlat
	}
	if style != StyleFlat && style != StyleHierarchical {
		return nil, fmt.Errorf("unknown enumeration style %q: want %q or %q", style, StyleFlat, StyleHierarchical)
	}
	if outputPath == "" {
		return nil, fmt.Errorf("enumeration output path is required")
	}

	args := []string{opts.Project.Action()}
	args = append(args, opts.Project.args()...)
	args = append(args,
		"-destination", opts.Destination,
		"-enumerate-tests",
		"-test-enumeration-style", string(style),
		"-test-enumeration-format", "json",
		"-test-enumeration-output-path", outputPath,
	)
	args = append(args, opts.ExtraArgs...)
	return args, nil
}

// EnumerateCommand renders the enumeration invocation as a single line, for
// logs and --dry-run. The output path is a placeholder: the real one is a
// temporary file chosen at run time.
func EnumerateCommand(opts EnumerateOptions) (string, error) {
	args, err := EnumerateArgs(opts, "$TMPDIR/gxcui-enumerate/tests.json")
	if err != nil {
		return "", err
	}
	return exec.Command{Name: Binary, Args: args}.String(), nil
}

// FlatPlan is one test plan's worth of enumerated tests.
type FlatPlan struct {
	TestPlan      string           `json:"testPlan"`
	EnabledTests  []TestIdentifier `json:"enabledTests"`
	DisabledTests []TestIdentifier `json:"disabledTests"`
}

// TestIdentifier is a single "Target/Class/method()" identifier, in the exact
// form -only-testing accepts.
type TestIdentifier struct {
	Identifier string `json:"identifier"`
}

// FlatEnumeration is the parsed result of a flat enumeration run.
type FlatEnumeration struct {
	Errors []json.RawMessage `json:"errors"`
	Values []FlatPlan        `json:"values"`
}

// ErrorMessages renders the errors xcodebuild reported. The entries are objects
// whose shape is not documented, so a "message" field is used when present and
// the raw JSON otherwise.
func (e FlatEnumeration) ErrorMessages() []string {
	msgs := make([]string, 0, len(e.Errors))
	for _, raw := range e.Errors {
		var obj struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.Message != "" {
			msgs = append(msgs, obj.Message)
			continue
		}
		msgs = append(msgs, string(raw))
	}
	return msgs
}

// ParseFlat decodes the JSON written by a flat enumeration run.
func ParseFlat(data []byte) (*FlatEnumeration, error) {
	var out FlatEnumeration
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse test enumeration: %w", err)
	}
	if msgs := out.ErrorMessages(); len(msgs) > 0 {
		return &out, fmt.Errorf("test enumeration reported errors: %s", strings.Join(msgs, "; "))
	}
	return &out, nil
}

// Enumerate runs xcodebuild in enumeration mode and returns the parsed result.
//
// The JSON is written to a temporary file that is removed before returning; a
// caller that wants to keep the raw output should copy it out of the returned
// value instead.
func Enumerate(ctx context.Context, r exec.Runner, opts EnumerateOptions) (*FlatEnumeration, error) {
	if opts.Style == StyleHierarchical {
		return nil, fmt.Errorf("hierarchical enumeration is not parsed yet: use %q", StyleFlat)
	}

	dir, err := os.MkdirTemp("", "gxcui-enumerate-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir for enumeration: %w", err)
	}
	defer os.RemoveAll(dir)
	outputPath := filepath.Join(dir, "tests.json")

	args, err := EnumerateArgs(opts, outputPath)
	if err != nil {
		return nil, err
	}

	res, err := r.Run(ctx, exec.Command{Name: Binary, Args: args, Stdout: opts.Stdout, Stderr: opts.Stderr})
	if err != nil {
		return nil, fmt.Errorf("enumerate tests: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, failureError("enumerate tests", res)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("enumerate tests: xcodebuild wrote no output to %s: %w", outputPath, err)
	}
	return ParseFlat(data)
}
