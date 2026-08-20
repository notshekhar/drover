package lsp

import "runtime"

// goosName and goarchName exist so registry.go can name the platform without
// shadowing the runtime package, which it also has a local variable for.
func goosName() string   { return runtime.GOOS }
func goarchName() string { return runtime.GOARCH }

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
