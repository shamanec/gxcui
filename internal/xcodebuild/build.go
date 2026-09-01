package xcodebuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// BuildOptions describes a build-for-testing run.
type BuildOptions struct {
	Project Project
	// Destination narrows the build to one platform. Without it xcodebuild
	// builds for the scheme's default, which may not be a simulator at all.
	Destination string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
}

// BuildArgs renders the argument vector for a build-for-testing run.
func BuildArgs(opts BuildOptions) ([]string, error) {
	if err := opts.Project.Validate(); err != nil {
		return nil, err
	}
	if opts.Project.prebuilt() {
		return nil, fmt.Errorf("nothing to build: the input is already built")
	}
	if opts.Project.DerivedDataPath == "" {
		return nil, fmt.Errorf("derived data path is required to build: gxcui needs a known place to find the .xctestrun file")
	}

	args := []string{ActionBuildForTesting}
	args = append(args, opts.Project.args()...)
	if opts.Destination != "" {
		args = append(args, "-destination", opts.Destination)
	}
	args = append(args, opts.ExtraArgs...)
	return args, nil
}

// BuildForTesting compiles the project and its tests, producing an .xctestrun
// file under the derived data path. Its path is returned.
//
// Building once and running every batch from the resulting .xctestrun is the
// core of gxcui's parallelism: test-without-building does no compilation, so any
// number of them can run at the same time without contending on a build graph.
func BuildForTesting(ctx context.Context, r exec.Runner, opts BuildOptions) (string, error) {
	args, err := BuildArgs(opts)
	if err != nil {
		return "", err
	}

	res, err := r.Run(ctx, exec.Command{Name: Binary, Args: args})
	if err != nil {
		return "", fmt.Errorf("build for testing: %w", err)
	}
	if res.ExitCode != 0 {
		return "", failureError("build for testing", res)
	}

	return FindXCTestRun(opts.Project.DerivedDataPath, opts.Project.TestPlan)
}

// FindXCTestRun locates the .xctestrun file a build produced.
//
// A build emits one file per test plan, so when the project has several the
// caller must say which plan it meant. Guessing would silently run the wrong
// set of tests, so ambiguity is an error.
func FindXCTestRun(derivedDataPath, testPlan string) (string, error) {
	dir := filepath.Join(derivedDataPath, "Build", "Products")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("find .xctestrun: %w", err)
	}

	var found []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".xctestrun") {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(found)

	switch {
	case len(found) == 0:
		return "", fmt.Errorf("find .xctestrun: none in %s: did build-for-testing succeed?", dir)
	case len(found) == 1:
		return found[0], nil
	}

	if testPlan != "" {
		// build-for-testing names the file <Scheme>_<TestPlan>_<sdk>-<arch>.xctestrun.
		var matched []string
		for _, path := range found {
			if strings.Contains(filepath.Base(path), "_"+testPlan+"_") {
				matched = append(matched, path)
			}
		}
		if len(matched) == 1 {
			return matched[0], nil
		}
	}

	names := make([]string, 0, len(found))
	for _, path := range found {
		names = append(names, filepath.Base(path))
	}
	return "", fmt.Errorf("find .xctestrun: %d candidates in %s (%s): set the test plan to choose one",
		len(found), dir, strings.Join(names, ", "))
}
