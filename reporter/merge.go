package reporter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shamanec/gxcui/internal/exec"
)

// Merge combines result bundles into one at outputPath, which is returned.
//
// A merged bundle is the natural hand-off to anything that consumes a single
// .xcresult — the HTML report, Xcode itself, or CI archiving — and it keeps the
// per-device results distinct rather than flattening them.
//
// xcresulttool requires two or more inputs, so a lone bundle is copied into
// place instead of merged.
func (r *Reporter) Merge(ctx context.Context, bundles []string, outputPath string) (string, error) {
	switch len(bundles) {
	case 0:
		return "", fmt.Errorf("merge result bundles: nothing to merge")
	case 1:
		if err := copyBundle(bundles[0], outputPath); err != nil {
			return "", err
		}
		return outputPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", fmt.Errorf("merge result bundles: %w", err)
	}
	// xcresulttool refuses to write over an existing bundle.
	if err := os.RemoveAll(outputPath); err != nil {
		return "", fmt.Errorf("merge result bundles: %w", err)
	}

	args := append([]string{"xcresulttool", "merge", "--output-path", outputPath}, bundles...)
	res, err := r.runner.Run(ctx, exec.Command{Name: "xcrun", Args: args})
	if err != nil {
		return "", fmt.Errorf("merge result bundles: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("merge result bundles: xcresulttool exited %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return outputPath, nil
}

// copyBundle duplicates a result bundle directory tree.
func copyBundle(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("copy result bundle: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("copy result bundle: %w", err)
	}

	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		return fmt.Errorf("copy result bundle: %w", err)
	}
	return nil
}
