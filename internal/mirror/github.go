package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PerPage is GitHub's maximum page size. Asking for less would only mean more
// round trips against the same rate limit.
const PerPage = 100

// Fetcher performs one GitHub REST GET and returns the raw body.
//
// It is an interface for one reason: a test must be able to run the whole
// mirror without a network or a token.
type Fetcher interface {
	Get(ctx context.Context, slug Slug, path string, params url.Values) ([]byte, error)
	// Describe names the credential path in use, for `drover get repository`.
	Describe() string
}

// RateLimitError is returned when GitHub says to come back later. It is a
// state, not a failure: the mirror records it and resumes at Reset.
type RateLimitError struct {
	Reset time.Time
}

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "github rate limit reached"
	}
	return fmt.Sprintf("github rate limit reached; it resets at %s", e.Reset.UTC().Format(time.RFC3339))
}

// NewFetcher picks how to reach GitHub.
//
// `gh` first, for the same reason git, go install and kubectl are shelled out
// to elsewhere in drover: it already solves auth, including enterprise SSO and
// token refresh, and solving auth again here would be the whole cost of this
// feature. A bare token is the fallback for a machine without it.
func NewFetcher(client *http.Client) Fetcher {
	if path, err := exec.LookPath("gh"); err == nil {
		return &ghCLI{bin: path}
	}
	return &restFetcher{client: client, token: firstEnv("GITHUB_TOKEN", "GH_TOKEN")}
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// --- gh ---

type ghCLI struct{ bin string }

func (g *ghCLI) Describe() string { return "gh" }

func (g *ghCLI) Get(ctx context.Context, slug Slug, path string, params url.Values) ([]byte, error) {
	target := path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	args := []string{"api", "--method", "GET", target}
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Env = append(os.Environ(), "GH_PAGER=", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	if slug.Host != "github.com" {
		cmd.Env = append(cmd.Env, "GH_HOST="+slug.Host)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "rate limit") {
			return nil, &RateLimitError{}
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gh api %s: %s", target, firstLine(msg))
	}
	return out, nil
}

// --- rest ---

type restFetcher struct {
	client *http.Client
	token  string
}

func (r *restFetcher) Describe() string {
	if r.token == "" {
		return "unauthenticated (set GITHUB_TOKEN, or install gh)"
	}
	return "GITHUB_TOKEN"
}

func (r *restFetcher) Get(ctx context.Context, slug Slug, path string, params url.Values) ([]byte, error) {
	base := "https://api.github.com/"
	if slug.Host != "github.com" {
		// GitHub Enterprise hangs its API off the same host under /api/v3.
		base = "https://" + slug.Host + "/api/v3/"
	}
	target := base + strings.TrimPrefix(path, "/")
	if len(params) > 0 {
		target += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	client := r.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case isRateLimited(resp):
		return nil, &RateLimitError{Reset: resetTime(resp)}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("github refused the request (%d); set GITHUB_TOKEN or install gh", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("github has no %s for %s (private repository with no token?)", path, slug)
	default:
		return nil, fmt.Errorf("github returned %d for %s: %s", resp.StatusCode, path, firstLine(string(body)))
	}
}

// isRateLimited distinguishes the limit from an ordinary permission failure.
// Both are 403; only one of them is worth waiting out.
func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
}

func resetTime(resp *http.Response) time.Time {
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	return time.Time{}
}

// maxBody caps one API page. A page of 100 issues with long bodies is large
// but bounded; anything past this is a server misbehaving.
const maxBody = 32 << 20

func readCapped(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) > maxBody {
			return nil, errors.New("github response is implausibly large")
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
	}
}

func decodePage[T any](body []byte) ([]T, error) {
	var out []T
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("github returned something that is not a list: %w", err)
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
