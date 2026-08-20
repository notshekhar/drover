package lsp

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Acquisition limits.
const (
	// stallTimeout is reset on every chunk received, rather than capping the
	// whole transfer. Eclipse serves the 28MB jdtls snapshot at around
	// 120KB/s -- four minutes of perfectly healthy download that a total
	// timeout would kill for no reason.
	stallTimeout = 60 * time.Second

	// maxArchive bounds what a compromised or confused mirror can make us
	// write to disk.
	maxArchive = 400 << 20

	versionTimeout = 10 * time.Second
	installTimeout = 15 * time.Minute
)

// Acquirer finds language servers, installing them when it must.
//
// Everything it installs goes under Dir, never into a checkout and never onto
// the user's PATH. drover owning its own copies is what lets it guarantee a
// version.
type Acquirer struct {
	Dir string

	// NoInstall turns off every route that reaches the network, leaving only
	// what is already on the machine.
	NoInstall bool

	mu       sync.Mutex
	inflight map[string]*sync.Once
	resolved map[string]*Resolved
}

// Resolved is a server that is ready to launch.
type Resolved struct {
	Key     string
	Bin     string
	Args    []string
	Env     []string
	Version string
	// Source says where the binary came from, which is the first thing anyone
	// debugging a wrong answer wants to know.
	Source string
}

// NewAcquirer returns an acquirer rooted at dir.
func NewAcquirer(dir string) *Acquirer {
	return &Acquirer{Dir: dir, inflight: map[string]*sync.Once{}, resolved: map[string]*Resolved{}}
}

// DefaultDir is where servers live: ~/.drover/servers.
//
// Deliberately not under the data directory. Servers are large, per-machine
// and per-architecture; the data directory is drover's state and someone may
// reasonably move, copy or wipe it.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "drover-servers")
	}
	return filepath.Join(home, ".drover", "servers")
}

func (a *Acquirer) binDir() string { return filepath.Join(a.Dir, "bin") }

func (a *Acquirer) serverDir(key string) string { return filepath.Join(a.Dir, key) }

// Ensure returns a launchable server, installing it if that is allowed.
//
// Installation is expensive and concurrent tool calls will ask for the same
// server at the same time, so each key installs at most once per process.
func (a *Acquirer) Ensure(ctx context.Context, def *Definition) (*Resolved, error) {
	a.mu.Lock()
	if got, ok := a.resolved[def.Key]; ok {
		a.mu.Unlock()
		return got, nil
	}
	once, ok := a.inflight[def.Key]
	if !ok {
		once = &sync.Once{}
		a.inflight[def.Key] = once
	}
	a.mu.Unlock()

	var result *Resolved
	var err error
	once.Do(func() {
		result, err = a.ensure(ctx, def)
		if err == nil {
			a.mu.Lock()
			a.resolved[def.Key] = result
			a.mu.Unlock()
		} else {
			// A failed install should be retryable -- the network comes back.
			a.mu.Lock()
			delete(a.inflight, def.Key)
			a.mu.Unlock()
		}
	})
	if result != nil || err != nil {
		return result, err
	}

	a.mu.Lock()
	got, ok := a.resolved[def.Key]
	a.mu.Unlock()
	if ok {
		return got, nil
	}
	return nil, fmt.Errorf("the %s server could not be started", def.Language)
}

func (a *Acquirer) ensure(ctx context.Context, def *Definition) (*Resolved, error) {
	// The toolchain gate runs first, before anything is downloaded. jdtls is
	// 50MB and needs a Java 21 runtime; finding that out afterwards is a waste
	// of somebody's bandwidth and a much worse error message.
	if err := a.checkRequirements(ctx, def); err != nil {
		return nil, err
	}

	if found := a.find(ctx, def); found != nil {
		return a.launchable(def, found)
	}
	if a.NoInstall {
		return nil, fmt.Errorf("no %s language server is installed, and installing is turned off", def.Language)
	}

	if err := a.install(ctx, def); err != nil {
		return nil, err
	}
	found := a.find(ctx, def)
	if found == nil {
		return nil, fmt.Errorf("the %s server installed but its binary could not be found afterwards", def.Language)
	}
	return a.launchable(def, found)
}

// CheckRequirements verifies the toolchains a server leans on, without
// installing anything. Exported so status can ask "could this start" without
// starting it.
func (a *Acquirer) CheckRequirements(ctx context.Context, def *Definition) error {
	return a.checkRequirements(ctx, def)
}

// Find reports an already-installed server, or nil. It never reaches the
// network.
func (a *Acquirer) Find(ctx context.Context, def *Definition) *Resolved {
	f := a.find(ctx, def)
	if f == nil {
		return nil
	}
	res, err := a.launchable(def, f)
	if err != nil {
		return nil
	}
	return res
}

// checkRequirements verifies the toolchains a server leans on.
func (a *Acquirer) checkRequirements(ctx context.Context, def *Definition) error {
	for _, need := range def.Requires {
		path, err := exec.LookPath(need)
		if err != nil {
			return fmt.Errorf("the %s server needs %s on PATH, and it is not there", def.Language, need)
		}
		min, ok := def.RequiresMinVersion[need]
		if !ok {
			continue
		}
		major, err := majorVersion(ctx, path, versionArgs(need)...)
		if err != nil {
			// macOS ships a /usr/bin/java stub that exists, runs, and reports
			// no runtime at all. Present on PATH is not the same as usable, so
			// this fails closed.
			return fmt.Errorf("the %s server needs %s %d or newer; %s is on PATH but did not report a usable version (%v)", def.Language, need, min, path, err)
		}
		if major < min {
			return fmt.Errorf("the %s server needs %s %d or newer; %s reports %d", def.Language, need, min, path, major)
		}
	}
	return nil
}

// versionArgs is how each toolchain is asked. Java writes its version to
// stderr and spells the flag differently from everyone else.
func versionArgs(bin string) []string {
	if bin == "java" {
		return []string{"-version"}
	}
	return []string{"version"}
}

// find looks for an installed binary: drover's own copy first, then PATH.
//
// drover's copy wins because it is the one whose version we chose. A PATH
// binary is still preferred over installing another copy, but only if it
// passes the version gate.
func (a *Acquirer) find(ctx context.Context, def *Definition) *found {
	if def.NPM != nil {
		path := filepath.Join(a.serverDir(def.Key), def.NPM.Bin)
		if ok, version := a.usable(ctx, def, path); ok {
			return &found{path: path, version: version, source: "downloaded by drover from npm"}
		}
	}
	if def.Download != nil {
		if path := locateBinary(a.serverDir(def.Key), def.Download.Bin); path != "" {
			return &found{path: path, source: "downloaded by drover"}
		}
	}
	for _, name := range def.BinNames {
		path := filepath.Join(a.binDir(), name+exeSuffix())
		if ok, version := a.usable(ctx, def, path); ok {
			return &found{path: path, version: version, source: "installed by drover"}
		}
	}
	for _, name := range def.BinNames {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if ok, version := a.usable(ctx, def, path); ok {
			return &found{path: path, version: version, source: "found on PATH"}
		}
	}
	return nil
}

type found struct {
	path    string
	version string
	source  string
}

// usable checks that a binary exists and is new enough to speak LSP.
func (a *Acquirer) usable(ctx context.Context, def *Definition, path string) (bool, string) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false, ""
	}
	if def.MinMajorVersion == 0 {
		return true, ""
	}
	major, err := majorVersion(ctx, path, "--version")
	if err != nil || major < def.MinMajorVersion {
		return false, ""
	}
	return true, strconv.Itoa(major)
}

// launchable turns a located binary into a command line.
func (a *Acquirer) launchable(def *Definition, f *found) (*Resolved, error) {
	res := &Resolved{Key: def.Key, Bin: f.path, Version: f.version, Source: f.source}

	if def.Runtime != "java" {
		res.Args = append(res.Args, def.Args...)
		return res, nil
	}

	// A jar is not an executable. The JVM runs it, the JVM flags come before
	// -jar, and the launcher needs a config directory that matches the
	// architecture plus a scratch workspace of its own.
	java, err := exec.LookPath("java")
	if err != nil {
		return nil, errors.New("java is not on PATH")
	}
	configDir, err := a.javaConfigDir(def)
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Join(a.serverDir(def.Key), "workspaces", "default")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	res.Bin = java
	res.Args = append(res.Args, def.JVMArgs...)
	res.Args = append(res.Args, "-jar", f.path)
	for _, arg := range def.Args {
		arg = strings.ReplaceAll(arg, "{configDir}", configDir)
		arg = strings.ReplaceAll(arg, "{dataDir}", dataDir)
		res.Args = append(res.Args, arg)
	}
	return res, nil
}

// javaConfigDir picks jdtls's platform configuration directory.
//
// config_mac_arm and config_linux_arm really are in the tarball. Hardcoding
// config_mac hands Apple Silicon the x86 configuration, so probe for the arm
// variant first and fall back.
func (a *Acquirer) javaConfigDir(def *Definition) (string, error) {
	root := a.serverDir(def.Key)
	var names []string
	switch runtime := goosName(); runtime {
	case "darwin":
		names = []string{"config_mac_arm", "config_mac"}
	case "windows":
		names = []string{"config_win"}
	default:
		names = []string{"config_linux_arm", "config_linux"}
	}
	if goarchName() != "arm64" {
		// Drop the arm variant rather than preferring it on an x86 box.
		names = names[len(names)-1:]
	}
	for _, name := range names {
		path := filepath.Join(root, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("no jdtls configuration directory (looked for %s in %s)", strings.Join(names, ", "), root)
}

// --- installation ---

func (a *Acquirer) install(ctx context.Context, def *Definition) error {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	switch {
	case def.NPM != nil:
		return a.installNPM(ctx, def)
	case def.GoInstall != "":
		return a.installGo(ctx, def)
	case def.Download != nil:
		return a.installDownload(ctx, def)
	}
	return fmt.Errorf("no %s language server is installed, and drover has no way to install one", def.Language)
}

// installGo shells out to the user's Go toolchain, into drover's own bin
// directory rather than theirs.
func (a *Acquirer) installGo(ctx context.Context, def *Definition) error {
	if err := os.MkdirAll(a.binDir(), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "install", def.GoInstall)
	cmd.Env = append(os.Environ(), "GOBIN="+a.binDir())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install %s: %s", def.GoInstall, firstLine(stderr.String(), err))
	}
	return nil
}

// installNPM fetches a package straight from the npm registry.
//
// The registry is plain HTTPS and a package is a gzipped tar, so this needs
// neither npm nor a JavaScript runtime -- which matters, because drover is one
// static binary and requiring Node to read a .ts file would be absurd.
func (a *Acquirer) installNPM(ctx context.Context, def *Definition) error {
	pkg := def.NPM.packageName()
	meta := "https://registry.npmjs.org/" + strings.ReplaceAll(pkg, "/", "%2F")

	body, err := get(ctx, meta)
	if err != nil {
		return fmt.Errorf("ask npm about %s: %w", pkg, err)
	}
	defer body.Close()

	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return fmt.Errorf("npm's answer for %s could not be read: %w", pkg, err)
	}
	version := doc.DistTags["latest"]
	entry, ok := doc.Versions[version]
	if !ok || entry.Dist.Tarball == "" {
		return fmt.Errorf("npm lists no download for %s@%s -- is %s/%s a supported platform?", pkg, version, npmPlatform(), npmArch())
	}

	dir := a.serverDir(def.Key)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := a.fetchArchive(ctx, entry.Dist.Tarball, "tar.gz", dir); err != nil {
		return err
	}
	// Nothing in a tarball is guaranteed executable, and npm's own client sets
	// this bit itself rather than trusting the archive.
	return os.Chmod(filepath.Join(dir, def.NPM.Bin), 0o755)
}

func (a *Acquirer) installDownload(ctx context.Context, def *Definition) error {
	dir := a.serverDir(def.Key)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return a.fetchArchive(ctx, def.Download.URL, def.Download.Format, dir)
}

func (a *Acquirer) fetchArchive(ctx context.Context, url, format string, dir string) error {
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	switch format {
	case "tar.gz", "":
		return extractTarGz(stallReader(ctx, body), dir)
	default:
		return fmt.Errorf("archive format %q is not supported", format)
	}
}

func get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "drover")
	// No client timeout: the stall watchdog is the timeout, because a slow but
	// healthy transfer is not a failure.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return resp.Body, nil
}

// stallReader aborts a transfer that stops making progress, rather than one
// that is merely slow.
func stallReader(ctx context.Context, r io.Reader) io.Reader {
	return &watchdog{ctx: ctx, r: r, last: time.Now()}
}

type watchdog struct {
	ctx  context.Context
	r    io.Reader
	last time.Time
}

func (w *watchdog) Read(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.r.Read(p)
	if n > 0 {
		w.last = time.Now()
	} else if time.Since(w.last) > stallTimeout {
		return n, fmt.Errorf("the download stalled for %s", stallTimeout)
	}
	return n, err
}

// extractTarGz unpacks into dir.
//
// Every entry's path is checked against the destination. An archive is
// somebody else's data, and "../../.ssh/authorized_keys" is a perfectly valid
// tar entry name.
func extractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the download is not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(io.LimitReader(gz, maxArchive))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
		// Symlinks and everything else are skipped: no server we ship needs
		// one, and a link is the other half of a traversal escape.
	}
}

// safeJoin refuses an entry that would land outside dir.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("the archive contains an entry outside itself: %q", name)
	}
	target := filepath.Join(dir, clean)
	rel, err := filepath.Rel(dir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("the archive contains an entry outside itself: %q", name)
	}
	return target, nil
}

// locateBinary finds the binary inside an unpacked archive: the declared path,
// then that path as a glob, then a bounded search by basename.
//
// Never trust the archive layout. Projects really do wrap their binary in a
// versioned directory, and change their minds about it between releases.
func locateBinary(root, declared string) string {
	if declared == "" {
		return ""
	}
	direct := filepath.Join(root, declared)
	if info, err := os.Stat(direct); err == nil && !info.IsDir() {
		return direct
	}
	if matches, err := filepath.Glob(direct); err == nil && len(matches) > 0 {
		return matches[len(matches)-1] // the newest, for a versioned name
	}

	base := filepath.Base(declared)
	var hit string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return nil
		}
		if ok, _ := filepath.Match(base, d.Name()); ok {
			hit = path
		}
		return nil
	})
	return hit
}

// --- versions ---

var versionNumber = regexp.MustCompile(`(\d+)\.(\d+)`)

// majorVersion runs a binary's version flag and reads the first number.
//
// Output goes to stdout for most tools and stderr for java, so both are read.
func majorVersion(ctx context.Context, bin string, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil && strings.TrimSpace(text) == "" {
		return 0, err
	}
	m := versionNumber.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("no version in %q", strings.TrimSpace(firstLineOf(text)))
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	// Java's "1.8.0" is Java 8; every release since says 11, 17, 21 plainly.
	if major == 1 {
		if minor, err := strconv.Atoi(m[2]); err == nil {
			return minor, nil
		}
	}
	return major, nil
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstLine(stderr string, fallback error) string {
	if s := strings.TrimSpace(firstLineOf(stderr)); s != "" {
		return s
	}
	return fallback.Error()
}
