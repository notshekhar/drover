package lsp

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Definition describes one language server declaratively.
//
// Declarative rather than a spawn function per server, so that adding a
// language is data rather than code. opencode hand-writes an imperative
// launcher for each and spends 1983 lines doing it.
type Definition struct {
	Key        string
	Language   string   // what to call it in a message
	Extensions []string // lowercase, with the dot

	// Runtime is "native" for an executable, or "java" for a jar that the
	// user's own JVM has to run.
	Runtime  string
	BinNames []string
	Args     []string

	// JVMArgs go BEFORE -jar. After it they are arguments to the application,
	// not to the JVM, so the system properties jdtls reads its product id from
	// would silently never be set.
	JVMArgs []string

	// RootMarkers identify a project root; the nearest ancestor of the file
	// wins, so a monorepo gets one server per package rather than one server
	// that has to hold all of them.
	RootMarkers []string

	// DisqualifyMarkers stand a server down where another owns the language:
	// a deno.json means TypeScript is Deno's business.
	DisqualifyMarkers []string

	// Requires are binaries that must be on PATH for the server to work at
	// all, with an optional minimum major version.
	Requires           []string
	RequiresMinVersion map[string]int

	// MinMajorVersion is the minimum major version a DISCOVERED binary must
	// report before we will speak LSP to it.
	MinMajorVersion int

	// Acquisition routes, tried when nothing is installed.
	GoInstall string
	NPM       *NPMSpec
	Download  *DownloadSpec

	// InitOptions are passed as initializationOptions in the handshake.
	InitOptions map[string]any
}

// NPMSpec fetches a package straight from the npm registry.
//
// No npm, no node, no bun: the registry is HTTPS and a package is a gzipped
// tar, both of which are in Go's standard library. {platform} and {arch} are
// npm's names for them, not Go's.
type NPMSpec struct {
	Package string // e.g. "@typescript/typescript-{platform}-{arch}"
	Bin     string // path to the binary inside the archive
}

// DownloadSpec fetches a prebuilt release archive.
type DownloadSpec struct {
	URL    string
	Format string // "tar.gz" or "zip"
	Bin    string // path within the archive; may be a glob
}

// Definitions are the languages drover speaks.
//
// Three, on purpose. Every entry here is a server somebody has to be able to
// install, diagnose and explain; a list of thirty-seven is a list of
// thirty-seven ways to be silently unavailable.
var Definitions = []*Definition{
	{
		// TypeScript 7 is a native Go binary that speaks LSP itself. No
		// typescript-language-server wrapper and no JS runtime in front of it.
		//
		// Deliberately NOT preferring a checkout's own node_modules/.bin/tsc.
		// tsc has been TypeScript's compiler for a decade and only v7 answers
		// --lsp, so preferring a project's local copy launches a v5 that fails
		// the handshake and takes the whole language down with it. drover's
		// checkouts are mirrors nobody ran an install in, so the local copy is
		// not there anyway -- and shipping our own pinned 7 makes the whole
		// class of failure unreachable.
		Key:               "typescript",
		Language:          "TypeScript",
		Extensions:        []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"},
		Runtime:           "native",
		BinNames:          []string{"tsgo", "tsc"},
		Args:              []string{"--lsp", "--stdio"},
		MinMajorVersion:   7,
		RootMarkers:       []string{"tsconfig.json", "jsconfig.json", "package.json"},
		DisqualifyMarkers: []string{"deno.json", "deno.jsonc"},
		NPM: &NPMSpec{
			Package: "@typescript/typescript-{platform}-{arch}",
			Bin:     filepath.Join("package", "lib", "tsc"),
		},
	},
	{
		// gopls has no prebuilt releases, so a machine with no Go toolchain
		// gets a clear refusal rather than a silent absence.
		Key:         "go",
		Language:    "Go",
		Extensions:  []string{".go"},
		Runtime:     "native",
		BinNames:    []string{"gopls"},
		RootMarkers: []string{"go.mod", "go.work"},
		Requires:    []string{"go"},
		GoInstall:   "golang.org/x/tools/gopls@latest",
	},
	{
		// Not an executable: an Equinox launcher jar run by the user's own JVM.
		Key:                "java",
		Language:           "Java",
		Extensions:         []string{".java"},
		Runtime:            "java",
		BinNames:           []string{"jdtls"},
		RootMarkers:        []string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", ".project"},
		Requires:           []string{"java"},
		RequiresMinVersion: map[string]int{"java": 21},
		JVMArgs: []string{
			"-Declipse.application=org.eclipse.jdt.ls.core.id1",
			"-Dosgi.bundles.defaultStartLevel=4",
			"-Declipse.product=org.eclipse.jdt.ls.core.product",
			"-Xmx1G",
			"--add-modules=ALL-SYSTEM",
			// One argv element each. "--add-opens a=b" as a single string is
			// read by the JVM as a flag literally named "--add-opens a=b".
			"--add-opens=java.base/java.util=ALL-UNNAMED",
			"--add-opens=java.base/java.lang=ALL-UNNAMED",
		},
		// {configDir} is arch-specific and {dataDir} is a scratch workspace,
		// both filled in once the archive is unpacked. The data directory sits
		// under drover's own servers folder, never inside a checkout -- jdtls
		// writes to it, and checkouts stay untouched.
		Args: []string{"-configuration", "{configDir}", "-data", "{dataDir}"},
		Download: &DownloadSpec{
			// A rolling snapshot; Eclipse publishes no version index for it.
			URL:    "https://www.eclipse.org/downloads/download.php?file=/jdtls/snapshots/jdt-language-server-latest.tar.gz",
			Format: "tar.gz",
			Bin:    filepath.Join("plugins", "org.eclipse.equinox.launcher_*.jar"),
		},
	},
}

// DefinitionFor returns the server that handles a path, or nil.
//
// One server per language here, so a file has at most one. loop returns a list
// because a type checker and a linter both answer for .ts and their
// diagnostics are complementary; drover offers navigation, where two answers
// to "where is this defined" is a worse result than one.
func DefinitionFor(path string) *Definition {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return nil
	}
	for _, def := range Definitions {
		for _, e := range def.Extensions {
			if e == ext {
				return def
			}
		}
	}
	return nil
}

// DefinitionByKey looks one up by key.
func DefinitionByKey(key string) *Definition {
	for _, def := range Definitions {
		if def.Key == key {
			return def
		}
	}
	return nil
}

// Languages lists what drover can speak, for a message.
func Languages() []string {
	out := make([]string, 0, len(Definitions))
	for _, def := range Definitions {
		out = append(out, def.Language)
	}
	return out
}

// npmPlatform and npmArch translate Go's names into npm's, which are not the
// same: GOOS "windows" is npm "win32", and GOARCH "amd64" is npm "x64".
func npmPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func npmArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

func (s *NPMSpec) packageName() string {
	name := strings.ReplaceAll(s.Package, "{platform}", npmPlatform())
	return strings.ReplaceAll(name, "{arch}", npmArch())
}
