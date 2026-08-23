package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	installShURL  = "https://raw.githubusercontent.com/" + repoSlug + "/main/install.sh"
	installPs1URL = "https://raw.githubusercontent.com/" + repoSlug + "/main/install.ps1"
	releasesAPI   = "https://api.github.com/repos/" + repoSlug + "/releases/latest"
	releasesHTML  = "https://github.com/" + repoSlug + "/releases/latest"
)

// repoSlug is where releases come from. The installer honours the same
// override, so a fork can upgrade from itself.
const repoSlug = "notshekhar/drover"

// installMarker is written by install.sh next to the binary. Its absence means
// drover arrived some other way -- go install, a package manager, a build from
// source -- and re-running the installer would replace that with something the
// original method knows nothing about.
const installMarker = ".install-method"

func cmdUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		forceFlag   = fs.Bool("force", false, "reinstall even when already up to date")
		versionFlag = fs.String("version", "", "install a specific release, e.g. v0.4.0")
		checkFlag   = fs.Bool("check", false, "report whether a newer release exists and exit")
	)
	flags, _ := splitArgs(args, map[string]bool{"version": true})
	if err := fs.Parse(flags); err != nil {
		return err
	}

	slug := repoSlug
	if env := os.Getenv("DROVER_REPO_SLUG"); env != "" {
		slug = env
	}

	fmt.Printf("▶ Checking for updates (current %s)\n", Version)
	latest, err := latestTag(ctx, slug)
	switch {
	case err != nil && *versionFlag == "":
		// Not fatal: the installer resolves the tag itself, and a network
		// hiccup here should not stop someone who wants to reinstall.
		fmt.Fprintf(os.Stderr, "  could not reach GitHub (%v); running the installer anyway\n", err)
	case latest != "":
		fmt.Printf("  latest is %s\n", latest)
	}

	if *checkFlag {
		return reportCheck(latest)
	}

	if *versionFlag == "" && !*forceFlag && latest != "" && !versionGreater(latest, Version) {
		fmt.Printf("✓ Up to date\n")
		fmt.Printf("  drover upgrade --force to reinstall anyway\n")
		return nil
	}

	if err := checkInstallMethod(*forceFlag); err != nil {
		return err
	}

	target := *versionFlag
	if target == "" {
		target = latest
	}
	if target != "" && target != Version {
		fmt.Printf("▶ Upgrading %s → %s\n", Version, target)
	}

	if err := runInstaller(slug, target, *forceFlag); err != nil {
		return err
	}

	// The running engine keeps the code it started with; only the file on
	// disk changed. Saying so avoids the "I upgraded but it still says the old
	// version" confusion.
	fmt.Printf("\n  restart any running `drover serve` to pick this up\n")
	return nil
}

func reportCheck(latest string) error {
	switch {
	case latest == "":
		return fmt.Errorf("could not determine the latest release")
	case versionGreater(latest, Version):
		fmt.Printf("A newer release is available: %s (you have %s)\n", latest, Version)
		fmt.Printf("  drover upgrade\n")
		return nil
	default:
		fmt.Printf("✓ Up to date\n")
		return nil
	}
}

// latestTag resolves the newest release.
//
// The releases/latest redirect is tried first because it is not subject to the
// anonymous GitHub API rate limit (60 requests an hour per IP) that bites on
// shared networks and in CI. The API is the fallback.
func latestTag(ctx context.Context, slug string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Head("https://github.com/" + slug + "/releases/latest")
	if err == nil {
		defer resp.Body.Close()
		if tag := tagFromURL(resp.Request.URL.String()); tag != "" {
			return tag, nil
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+slug+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	apiResp, apiErr := client.Do(req)
	if apiErr != nil {
		return "", apiErr
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s", apiResp.Status)
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(apiResp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("no release found")
	}
	return body.TagName, nil
}

// tagFromURL pulls the tag out of a .../releases/tag/vX.Y.Z url.
func tagFromURL(url string) string {
	tag := url[strings.LastIndexByte(url, '/')+1:]
	if len(tag) > 1 && tag[0] == 'v' && tag[1] >= '0' && tag[1] <= '9' {
		return tag
	}
	return ""
}

// versionGreater reports whether a is a newer version than b. Both may carry a
// leading v, and "dev" is treated as older than everything so a build from
// source always sees an upgrade available.
func versionGreater(a, b string) bool {
	if b == "dev" || b == "" {
		return true
	}
	if a == "dev" || a == "" {
		return false
	}
	as, bs := splitVersion(a), splitVersion(b)
	for i := 0; i < 3; i++ {
		if as[i] != bs[i] {
			return as[i] > bs[i]
		}
	}
	return false
}

func splitVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Drop any pre-release or build suffix; comparing those properly is more
	// machinery than a self-updater needs.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out
		}
		out[i] = n
	}
	return out
}

// checkInstallMethod refuses to clobber an install the installer did not make.
//
// Someone who ran `go install` or used a package manager has a binary that
// their own tooling tracks; dropping the release build on top of it leaves two
// versions and no clear owner. --force says do it anyway.
func checkInstallMethod(force bool) error {
	exe, err := os.Executable()
	if err != nil || force {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), installMarker)); err == nil {
		return nil
	}

	return fmt.Errorf(`this drover was not installed by the installer (no %s beside %s)

  it probably came from `+"`go install`"+`, a package manager, or a build from source,
  and upgrading in place would leave two copies with no clear owner.

  upgrade it the way you installed it, or run:
    drover upgrade --force`, installMarker, exe)
}

// runInstaller re-runs the published installer, which is the same path a first
// install takes -- including its checksum verification and its progress bar,
// which is why stdio is inherited rather than captured.
func runInstaller(slug, version string, force bool) error {
	env := append(os.Environ(), "DROVER_REPO_SLUG="+slug)
	if force {
		env = append(env, "DROVER_FORCE=1")
	}
	if version != "" {
		env = append(env, "DROVER_VERSION="+version)
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
			"-Command", "irm "+installPs1URL+" | iex")
	} else {
		if _, err := exec.LookPath("curl"); err != nil {
			return fmt.Errorf("curl is required to upgrade and was not found")
		}
		cmd = exec.Command("bash", "-c", "curl -fsSL "+installShURL+" | bash")
	}

	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the installer failed: %w", err)
	}
	return nil
}
