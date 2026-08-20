// Package config reads and writes ~/.drover/config.yaml.
//
// The client uses it to find the engine; serve uses it to pick a listen
// address and to bootstrap. Apply appends to its apply: list, so the set of
// sources the engine has been fed is recorded rather than maintained by hand.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/atomicfile"
)

// Defaults.
const (
	DefaultListen = "127.0.0.1:7432"
	DefaultSync   = time.Hour
	FileName      = "config.yaml"
)

// Server is one engine the client knows how to reach.
type Server struct {
	URL string `yaml:"url"`
}

// Config is ~/.drover/config.yaml.
type Config struct {
	Current string            `yaml:"current,omitempty"`
	Servers map[string]Server `yaml:"servers,omitempty"`

	Listen string `yaml:"listen,omitempty"`
	Sync   string `yaml:"sync,omitempty"`

	// Apply is the list of paths applied at startup, before the server
	// accepts traffic. Apply appends to it.
	Apply []string `yaml:"apply,omitempty"`

	// unknown holds top-level keys this build does not recognise. They are
	// carried through a rewrite so a field written by a newer drover is not
	// silently dropped when an older one saves the file.
	unknown map[string]yaml.Node `yaml:"-"`

	// path is where this config was loaded from, so Save writes it back.
	path string `yaml:"-"`
}

// Path returns the file this config came from.
func (c *Config) Path() string { return c.path }

// DataDir resolves the data directory: --data-dir, else DROVER_DATA, else
// ~/.drover.
func DataDir(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if env := os.Getenv("DROVER_DATA"); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory to locate ~/.drover: %w", err)
	}
	return filepath.Join(home, ".drover"), nil
}

// Path resolves the config file: --config, else DROVER_CONFIG, else
// <data dir>/config.yaml.
func Path(flag, dataDir string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if env := os.Getenv("DROVER_CONFIG"); env != "" {
		return filepath.Abs(env)
	}
	return filepath.Join(dataDir, FileName), nil
}

// Load reads the config at path. A missing file is not an error: it returns
// defaults, so a first run works without the user creating anything.
func Load(path string) (*Config, error) {
	c := &Config{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// A first run must work without the user creating anything, and if
		// something later saves this config it should write a file that looks
		// hand-written rather than one holding a lone apply: list.
		return New(path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Capture anything this build does not know about, so Save can put it back.
	var all map[string]yaml.Node
	if err := yaml.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	known := map[string]bool{"current": true, "servers": true, "listen": true, "sync": true, "apply": true}
	for k, v := range all {
		if !known[k] {
			if c.unknown == nil {
				c.unknown = map[string]yaml.Node{}
			}
			c.unknown[k] = v
		}
	}

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Validate checks the fields that would otherwise fail much later.
func (c *Config) Validate() error {
	if c.Current != "" {
		if _, ok := c.Servers[c.Current]; !ok {
			return fmt.Errorf("current is %q but servers has no such entry", c.Current)
		}
	}
	for name, s := range c.Servers {
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("servers.%s.url is empty", name)
		}
	}
	if c.Sync != "" {
		if _, err := ParseSync(c.Sync); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	return nil
}

// ParseSync reads the server-wide default refresh interval. "0"/"never"
// disables the ticker for every repository that did not set its own.
func ParseSync(s string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default":
		return DefaultSync, nil
	case "never", "off", "0":
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration; write it with a unit, like 15m or 1h (or never)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}

// SyncInterval is the resolved server-wide default.
func (c *Config) SyncInterval() time.Duration {
	d, err := ParseSync(c.Sync)
	if err != nil {
		return DefaultSync
	}
	return d
}

// ListenAddr is the resolved bind address.
func (c *Config) ListenAddr() string {
	if c.Listen == "" {
		return DefaultListen
	}
	return c.Listen
}

// ServerURL resolves which engine the client should talk to: an explicit
// override, else DROVER_URL, else the current server, else the default.
func (c *Config) ServerURL(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("DROVER_URL"); env != "" {
		return env, nil
	}
	if c.Current != "" {
		s, ok := c.Servers[c.Current]
		if !ok {
			return "", fmt.Errorf("current is %q but servers has no such entry", c.Current)
		}
		return s.URL, nil
	}
	if len(c.Servers) == 1 {
		for _, s := range c.Servers {
			return s.URL, nil
		}
	}
	if len(c.Servers) > 1 {
		return "", errors.New("several servers are configured but current is not set; pick one with --server or set current")
	}
	return "http://" + DefaultListen, nil
}

// Remember adds a path to the apply list. It reports whether anything
// changed, so a caller can skip a pointless write.
//
// Paths are stored absolute with symlinks resolved, so the same file reached
// two ways is one entry rather than two.
func (c *Config) Remember(path string) (bool, error) {
	abs, err := canonical(path)
	if err != nil {
		return false, err
	}
	for _, existing := range c.Apply {
		if existing == abs {
			return false, nil
		}
		// A file inside a directory that is already applied is already
		// covered; adding it separately would apply it twice on boot.
		if isInDir(abs, existing) {
			return false, nil
		}
	}
	c.Apply = append(c.Apply, abs)
	return true, nil
}

// Forget drops a path from the apply list, reporting whether it was there.
func (c *Config) Forget(path string) (bool, error) {
	abs, err := canonical(path)
	if err != nil {
		// A path that no longer exists must still be forgettable -- that is
		// the main reason to forget one -- so fall back to a lexical clean.
		abs = filepath.Clean(path)
		if !filepath.IsAbs(abs) {
			if cwd, err := os.Getwd(); err == nil {
				abs = filepath.Join(cwd, abs)
			}
		}
	}
	var kept []string
	found := false
	for _, existing := range c.Apply {
		if existing == abs {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	c.Apply = kept
	return found, nil
}

func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// isInDir reports whether path sits under dir, which must itself be a
// directory for the answer to mean anything.
func isInDir(path, dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Save writes the config back atomically.
//
// This is a marshal, not a round-trip: comments and key order in the file are
// not preserved. Unknown top-level keys are, so a field from a newer drover
// survives a save by an older one.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config has no path to save to")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}

	// Merge known fields with carried-through unknown ones.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	out := map[string]yaml.Node{}
	for k, v := range c.unknown {
		out[k] = v
	}
	known, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	var knownMap map[string]yaml.Node
	if err := yaml.Unmarshal(known, &knownMap); err != nil {
		return err
	}
	for k, v := range knownMap {
		out[k] = v
	}

	// Deterministic order, with the fields people read first at the top.
	order := []string{"current", "servers", "listen", "sync", "apply"}
	rank := map[string]int{}
	for i, k := range order {
		rank[k] = i
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, oki := rank[keys[i]]
		rj, okj := rank[keys[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return keys[i] < keys[j]
		}
	})

	doc := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		v := out[k]
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&v,
		)
	}
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	return atomicfile.Write(c.path, []byte(buf.String()), 0o644)
}

// New returns a config with defaults filled in, for a first run.
func New(path string) *Config {
	return &Config{
		path:    path,
		Listen:  DefaultListen,
		Current: "local",
		Servers: map[string]Server{"local": {URL: "http://" + DefaultListen}},
	}
}
