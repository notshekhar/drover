// Package httpreq executes an HTTPRequest object against an Environment.
//
// The rule that shapes everything here: a caller may fill declared parameters
// and nothing else. Environment values and process-environment values are
// resolved server-side and are never reachable from a tool argument, so an
// agent cannot read a secret by asking for it as a parameter.
package httpreq

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// DefaultTimeout caps one request when the document does not.
const DefaultTimeout = 30 * time.Second

// MaxBodyBytes caps what is read back. A tool result heads for a context
// window, so an unbounded read helps nobody.
const MaxBodyBytes = 1 << 20 // 1 MiB

// Executor runs requests.
type Executor struct {
	HTTP   *http.Client
	Getenv func(string) (string, bool)
}

// New returns an Executor with sane defaults.
func New() *Executor {
	return &Executor{
		HTTP: &http.Client{
			Timeout: DefaultTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		Getenv: os.LookupEnv,
	}
}

// Request is one call: which object, which environment, and the caller's
// parameters.
type Request struct {
	Spec        *object.HTTPRequestSpec
	Environment *object.EnvironmentSpec
	// EnvName is the selected environment's name, for error messages.
	EnvName string
	Params  map[string]string

	// AllowUnsafeMethod permits a non-GET request. MCP never sets this; the
	// CLI sets it behind an explicit flag.
	AllowUnsafeMethod bool
}

// Response is what came back.
type Response struct {
	Status     int               `json:"status"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
	DurationMS int64             `json:"durationMs"`
}

// Do builds and sends the request.
func (e *Executor) Do(ctx context.Context, req Request) (*Response, error) {
	spec := req.Spec
	method := spec.NormalizedMethod()
	if !spec.IsSafe() && !req.AllowUnsafeMethod {
		return nil, fmt.Errorf("%s is not offered: drover only executes GET requests (the document may store others, but they are never called on your behalf)", method)
	}

	built, err := e.build(ctx, req)
	if err != nil {
		return nil, err
	}

	client := e.HTTP
	if client == nil {
		client = New().HTTP
	}
	if spec.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
		defer cancel()
		built = built.WithContext(ctx)
	}

	start := time.Now()
	resp, err := client.Do(built)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	truncated := len(body) > MaxBodyBytes
	if truncated {
		body = body[:MaxBodyBytes]
	}

	out := &Response{
		Status:     resp.StatusCode,
		URL:        built.URL.String(),
		Method:     method,
		Body:       string(body),
		Truncated:  truncated,
		DurationMS: time.Since(start).Milliseconds(),
		Headers:    map[string]string{},
	}
	// Only the headers that help an agent read the result. Echoing them all
	// risks handing back a Set-Cookie.
	for _, h := range []string{"Content-Type", "Content-Length", "Location"} {
		if v := resp.Header.Get(h); v != "" {
			out.Headers[h] = v
		}
	}
	return out, nil
}

// build turns the spec plus parameters into an http.Request.
func (e *Executor) build(ctx context.Context, req Request) (*http.Request, error) {
	spec := req.Spec

	if err := e.checkParams(req); err != nil {
		return nil, err
	}

	// The url is resolved with escaping, since a value lands in a path or a
	// query and must not be able to add segments or parameters of its own.
	urlResolver := e.resolver(req, func(kind object.PlaceholderKind, value string) string {
		if kind == object.FromParam {
			return url.PathEscape(value)
		}
		return value
	})
	rawURL, err := urlResolver.Resolve(spec.URL)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url %q is not valid after substitution: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("url %q must be http or https after substitution", rawURL)
	}

	// Query values are set through url.Values, which encodes them, so a
	// parameter cannot inject another parameter.
	plain := e.resolver(req, nil)
	q := parsed.Query()
	for _, p := range spec.Query {
		value, ok := req.Params[p.Name]
		if !ok || value == "" {
			if p.Default == "" {
				continue
			}
			resolved, err := plain.Resolve(p.Default)
			if err != nil {
				return nil, fmt.Errorf("query %s default: %w", p.Name, err)
			}
			value = resolved
		}
		q.Set(p.Name, value)
	}
	parsed.RawQuery = q.Encode()

	var bodyReader io.Reader
	var bodyLen int
	if spec.Body != nil && strings.TrimSpace(spec.Body.Template) != "" {
		rendered, err := plain.Resolve(spec.Body.Template)
		if err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		bodyReader = strings.NewReader(rendered)
		bodyLen = len(rendered)
	}

	built, err := http.NewRequestWithContext(ctx, spec.NormalizedMethod(), parsed.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	if bodyLen > 0 {
		built.ContentLength = int64(bodyLen)
	}

	for _, h := range spec.Headers {
		value, err := plain.Resolve(h.Value)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", h.Name, err)
		}
		// Validation already refuses newlines in the template; this catches a
		// resolved value that carries one.
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("header %s resolved to a value containing a newline", h.Name)
		}
		built.Header.Set(h.Name, value)
	}
	if spec.Body != nil && spec.Body.ContentType != "" {
		built.Header.Set("Content-Type", spec.Body.ContentType)
	}
	if built.Header.Get("User-Agent") == "" {
		built.Header.Set("User-Agent", "drover")
	}
	return built, nil
}

// checkParams rejects a call that is missing something required or that
// carries a name the document never declared.
//
// Refusing unknown parameters matters: a caller that can smuggle in an
// undeclared name is a caller probing for one the template happens to use.
func (e *Executor) checkParams(req Request) error {
	declared := map[string]bool{}
	for _, p := range req.Spec.PathParams {
		declared[p.Name] = true
	}
	for _, q := range req.Spec.Query {
		declared[q.Name] = true
	}

	var unknown []string
	for name := range req.Params {
		if !declared[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown parameter(s): %s", strings.Join(unknown, ", "))
	}

	var missing []string
	for _, name := range req.Spec.RequiredParams() {
		if v, ok := req.Params[name]; !ok || v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required parameter(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// resolver wires the three sources. A caller's parameters can only ever fill
// {name}; they are not consulted for {{name}} or ${NAME}.
func (e *Executor) resolver(req Request, escape func(object.PlaceholderKind, string) string) *object.Resolver {
	getenv := e.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	return &object.Resolver{
		EnvName: req.EnvName,
		Env: func(name string) (string, bool) {
			if req.Environment == nil {
				return "", false
			}
			return req.Environment.Lookup(name, getenv)
		},
		Process: getenv,
		Param: func(name string) (string, bool) {
			v, ok := req.Params[name]
			return v, ok
		},
		Escape: escape,
	}
}
