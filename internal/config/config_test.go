package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// canon mirrors what Remember does to a path. On macOS t.TempDir() hands back
// a /var path that is really a symlink to /private/var, so a test that
// compares raw paths would fail on the symlink resolution rather than on
// anything about the config.
func canon(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("a missing config must not be an error: %v", err)
	}
	if c.ListenAddr() != DefaultListen {
		t.Errorf("listen = %q, want %q", c.ListenAddr(), DefaultListen)
	}
	if c.SyncInterval() != DefaultSync {
		t.Errorf("sync = %v, want %v", c.SyncInterval(), DefaultSync)
	}
}

func TestLoadAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, `current: home
servers:
  home:
    url: http://127.0.0.1:9000
listen: 0.0.0.0:7432
sync: 15m
apply:
  - /work/repos
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr() != "0.0.0.0:7432" {
		t.Errorf("listen = %q", c.ListenAddr())
	}
	if c.SyncInterval() != 15*time.Minute {
		t.Errorf("sync = %v, want 15m", c.SyncInterval())
	}
	url, err := c.ServerURL("")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://127.0.0.1:9000" {
		t.Errorf("server url = %q", url)
	}
	if got, err := c.ServerURL("http://other:1"); err != nil || got != "http://other:1" {
		t.Errorf("override = %q, %v", got, err)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"current names a missing server": "current: home\nservers:\n  work:\n    url: http://x\n",
		"empty server url":               "servers:\n  home:\n    url: \"\"\n",
		"sync without a unit":            "sync: 30\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".yaml")
			write(t, path, body)
			if _, err := Load(path); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestParseSync(t *testing.T) {
	cases := map[string]time.Duration{"": DefaultSync, "1h": time.Hour, "30m": 30 * time.Minute, "never": 0, "off": 0, "0": 0}
	for in, want := range cases {
		got, err := ParseSync(in)
		if err != nil {
			t.Errorf("ParseSync(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSync(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"soon", "30", "-5m"} {
		if _, err := ParseSync(in); err == nil {
			t.Errorf("ParseSync(%q) succeeded, want an error", in)
		}
	}
}

// Apply records where objects came from, so the next serve reads the same
// sources without the user maintaining the list.
func TestRememberAppendsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := New(path)

	f := filepath.Join(dir, "repo.yaml")
	write(t, f, repoYAML)

	added, err := c.Remember(f)
	if err != nil || !added {
		t.Fatalf("Remember = %v, %v; want true, nil", added, err)
	}
	if len(c.Apply) != 1 || c.Apply[0] != canon(t, f) {
		t.Fatalf("apply = %v, want [%s]", c.Apply, canon(t, f))
	}

	// Same path again is not a second entry.
	added, err = c.Remember(f)
	if err != nil {
		t.Fatal(err)
	}
	if added || len(c.Apply) != 1 {
		t.Errorf("re-remembering duplicated the entry: %v", c.Apply)
	}

	// A relative path pointing at the same file is the same entry.
	cwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	added, _ = c.Remember("repo.yaml")
	if added || len(c.Apply) != 1 {
		t.Errorf("a relative path to the same file added a second entry: %v", c.Apply)
	}
}

// A file inside a directory that is already applied is already covered.
// Adding it again would apply the same document twice at boot.
func TestRememberSkipsFileUnderAppliedDir(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "config.yaml"))

	repos := filepath.Join(dir, "repos")
	f := filepath.Join(repos, "api.yaml")
	write(t, f, repoYAML)

	if added, err := c.Remember(repos); err != nil || !added {
		t.Fatalf("Remember(dir) = %v, %v", added, err)
	}
	added, err := c.Remember(f)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Errorf("a file under an applied directory was added separately: %v", c.Apply)
	}
}

func TestForget(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "config.yaml"))
	f := filepath.Join(dir, "repo.yaml")
	write(t, f, repoYAML)

	if _, err := c.Remember(f); err != nil {
		t.Fatal(err)
	}
	found, err := c.Forget(f)
	if err != nil || !found {
		t.Fatalf("Forget = %v, %v; want true, nil", found, err)
	}
	if len(c.Apply) != 0 {
		t.Errorf("apply = %v, want empty", c.Apply)
	}

	// A path that no longer exists must still be forgettable -- that is the
	// main reason someone reaches for forget.
	gone := filepath.Join(dir, "deleted.yaml")
	c.Apply = []string{gone}
	found, err = c.Forget(gone)
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(c.Apply) != 0 {
		t.Errorf("forgetting a deleted path failed: found=%v apply=%v", found, c.Apply)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := New(path)
	f := filepath.Join(dir, "repo.yaml")
	write(t, f, repoYAML)
	if _, err := c.Remember(f); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Apply) != 1 || got.Apply[0] != canon(t, f) {
		t.Errorf("apply = %v, want [%s]", got.Apply, canon(t, f))
	}
	if got.ListenAddr() != DefaultListen {
		t.Errorf("listen = %q", got.ListenAddr())
	}
	if got.Current != "local" {
		t.Errorf("current = %q", got.Current)
	}
}

// Comments do not survive a rewrite, which is documented. A field a newer
// drover wrote must survive, or saving from an older build silently loses it.
func TestSavePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write(t, path, `listen: 127.0.0.1:7432
futureFeature:
  enabled: true
  level: 3
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "futureFeature") || !strings.Contains(body, "level: 3") {
		t.Errorf("unknown key was dropped on save:\n%s", body)
	}
}

func TestSaveIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := New(path)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("Save left %q behind", e.Name())
		}
	}
}

func TestDataDirPrefersFlagThenEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DROVER_DATA", filepath.Join(dir, "fromenv"))

	got, err := DataDir(filepath.Join(dir, "fromflag"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "fromflag") {
		t.Errorf("DataDir(flag) = %q, want the flag to win", got)
	}

	got, err = DataDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "fromenv") {
		t.Errorf("DataDir(\"\") = %q, want DROVER_DATA", got)
	}
}

func TestServerURLNeedsCurrentWhenAmbiguous(t *testing.T) {
	c := &Config{Servers: map[string]Server{
		"a": {URL: "http://a"},
		"b": {URL: "http://b"},
	}}
	if _, err := c.ServerURL(""); err == nil {
		t.Fatal("two servers with no current must be an error, not a coin flip")
	}
	// One server needs no current.
	c = &Config{Servers: map[string]Server{"a": {URL: "http://a"}}}
	if got, err := c.ServerURL(""); err != nil || got != "http://a" {
		t.Errorf("ServerURL = %q, %v", got, err)
	}
}

const repoYAML = `apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
`
