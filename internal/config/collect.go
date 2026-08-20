package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CollectFiles expands one -f path into the yaml files it names.
//
// A file is itself, whatever its extension -- if someone points at it
// explicitly they mean it. A directory is the *.yaml and *.yml files directly
// inside it, not recursively, matching `kubectl apply -f dir`. Recursing
// would sweep up vendored fixtures and test data nobody meant to apply.
//
// Results are sorted, so a directory applies in a stable order and an error
// message names the same file every run.
func CollectFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if !info.IsDir() {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		return []string{abs}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !IsYAML(e.Name()) {
			continue
		}
		abs, err := filepath.Abs(filepath.Join(path, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no .yaml or .yml files", path)
	}
	sort.Strings(out)
	return out, nil
}

// CollectAll expands every path, keeping order and dropping a file reached
// twice -- via its directory and again by name -- so it is not applied twice.
func CollectAll(paths []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		files, err := CollectFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			key := f
			if resolved, err := filepath.EvalSymlinks(f); err == nil {
				key = resolved
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no files to apply")
	}
	return out, nil
}

// IsYAML reports whether a filename is one drover will pick up from a
// directory.
func IsYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
