package object

import (
	"fmt"
	"sort"
	"strings"
)

// Param is one caller-supplied value: a path segment, a query value, or a
// header. The description is not decoration -- it is what an agent reads to
// decide what to put here, so an empty one makes the tool unusable.
type Param struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
	Example     string `yaml:"example,omitempty"`
	Default     string `yaml:"default,omitempty"`
}

// Header is a fixed header. Its value may carry {{...}} and ${...}, but never
// {param}: letting a caller set a raw header is how you get request
// smuggling and stolen credentials.
type Header struct {
	Name        string `yaml:"name"`
	Value       string `yaml:"value"`
	Description string `yaml:"description,omitempty"`
}

// Body is a request body template.
type Body struct {
	ContentType string `yaml:"contentType,omitempty"`
	Template    string `yaml:"template,omitempty"`
}

// HTTPRequestSpec is one callable request.
type HTTPRequestSpec struct {
	Description string `yaml:"description,omitempty"`
	Method      string `yaml:"method"`
	URL         string `yaml:"url"`

	Environments       []string `yaml:"environments,omitempty"`
	DefaultEnvironment string   `yaml:"defaultEnvironment,omitempty"`

	PathParams []Param  `yaml:"pathParams,omitempty"`
	Query      []Param  `yaml:"query,omitempty"`
	Headers    []Header `yaml:"headers,omitempty"`
	Body       *Body    `yaml:"body,omitempty"`

	// TimeoutSeconds caps one execution. Zero means the engine default.
	TimeoutSeconds int `yaml:"timeoutSeconds,omitempty"`
}

// HTTPRequest decodes this object's spec as an HTTPRequestSpec.
func (o *Object) HTTPRequest() (*HTTPRequestSpec, error) {
	if o.Kind != KindHTTPRequest {
		return nil, fmt.Errorf("object is %s, not %s", o.Kind, KindHTTPRequest)
	}
	var spec HTTPRequestSpec
	if err := o.decodeSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// SafeMethods are the methods MCP will advertise and execute. Everything else
// may be stored but is never offered to an agent.
var SafeMethods = map[string]bool{"GET": true}

// knownMethods is what may be written down at all.
var knownMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true,
	"PATCH": true, "DELETE": true, "OPTIONS": true,
}

// Normalized returns the method in upper case.
func (s *HTTPRequestSpec) NormalizedMethod() string {
	return strings.ToUpper(strings.TrimSpace(s.Method))
}

// IsSafe reports whether this request may be offered to an agent.
func (s *HTTPRequestSpec) IsSafe() bool { return SafeMethods[s.NormalizedMethod()] }

// Validate checks everything that can be known without an Environment. The
// cross-object checks -- that named environments exist -- happen at apply,
// where the store is reachable.
func (s *HTTPRequestSpec) Validate() error {
	method := s.NormalizedMethod()
	if method == "" {
		return fmt.Errorf("spec.method is required")
	}
	if !knownMethods[method] {
		return fmt.Errorf("spec.method %q is not an HTTP method", s.Method)
	}
	if strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("spec.url is required")
	}
	if strings.ContainsAny(s.URL, " \t\n") {
		return fmt.Errorf("spec.url contains whitespace")
	}

	// The url must be absolute once the environment is substituted. If it
	// opens with a placeholder we cannot tell yet, and that is fine -- that is
	// exactly what {{baseUrl}} is for.
	if !strings.HasPrefix(s.URL, "{{") && !strings.HasPrefix(s.URL, "${") {
		if !strings.Contains(s.URL, "://") {
			return fmt.Errorf("spec.url %q is not absolute; give it a scheme or start it with {{baseUrl}}", s.URL)
		}
	}

	if err := s.validateParams(); err != nil {
		return err
	}
	if err := s.validateHeaders(); err != nil {
		return err
	}
	if err := s.validateEnvironments(); err != nil {
		return err
	}
	if s.TimeoutSeconds < 0 {
		return fmt.Errorf("spec.timeoutSeconds is negative")
	}
	return nil
}

// validateParams enforces the agreement between {name} in the url and the
// declared pathParams.
//
// This is the check that keeps an advertised tool honest. A {userId} with no
// declaration means the agent is never told to supply it; a declaration with
// no {userId} in the url means the agent is asked for something that goes
// nowhere. Both are silent failures at call time, so both fail at apply.
func (s *HTTPRequestSpec) validateParams() error {
	declared := map[string]bool{}
	for _, p := range s.PathParams {
		if err := validateVarName(p.Name); err != nil {
			return fmt.Errorf("spec.pathParams: %w", err)
		}
		if declared[p.Name] {
			return fmt.Errorf("spec.pathParams has %q twice", p.Name)
		}
		declared[p.Name] = true
		if strings.TrimSpace(p.Description) == "" {
			return fmt.Errorf("spec.pathParams.%s needs a description; it is what tells an agent what to put there", p.Name)
		}
	}

	inURL := map[string]bool{}
	for _, name := range PlaceholderNames(s.URL, FromParam) {
		inURL[name] = true
	}

	var undeclared, unused []string
	for name := range inURL {
		if !declared[name] {
			undeclared = append(undeclared, name)
		}
	}
	for name := range declared {
		if !inURL[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(unused)

	if len(undeclared) > 0 {
		return fmt.Errorf("spec.url uses {%s} but spec.pathParams does not declare it", strings.Join(undeclared, "}, {"))
	}
	if len(unused) > 0 {
		return fmt.Errorf("spec.pathParams declares %q but spec.url never uses {%s}", strings.Join(unused, ", "), strings.Join(unused, "}, {"))
	}

	seenQuery := map[string]bool{}
	for _, q := range s.Query {
		if err := validateVarName(q.Name); err != nil {
			return fmt.Errorf("spec.query: %w", err)
		}
		if seenQuery[q.Name] {
			return fmt.Errorf("spec.query has %q twice", q.Name)
		}
		seenQuery[q.Name] = true
		if strings.TrimSpace(q.Description) == "" {
			return fmt.Errorf("spec.query.%s needs a description", q.Name)
		}
		if q.Required && q.Default != "" {
			return fmt.Errorf("spec.query.%s is required and also has a default; one of those is wrong", q.Name)
		}
	}
	return nil
}

func (s *HTTPRequestSpec) validateHeaders() error {
	seen := map[string]bool{}
	for _, h := range s.Headers {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return fmt.Errorf("spec.headers has an entry with no name")
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			return fmt.Errorf("spec.headers has %q twice", name)
		}
		seen[lower] = true

		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(h.Value, "\r\n") {
			return fmt.Errorf("spec.headers.%s contains a newline", name)
		}
		// A caller-settable header is a request-smuggling and
		// credential-theft primitive. Headers may interpolate server-side
		// values only.
		for _, p := range ScanPlaceholders(h.Value) {
			if p.Kind == FromParam {
				return fmt.Errorf("spec.headers.%s uses {%s}; headers may only use {{environment}} or ${processEnv} values, never caller-supplied ones", name, p.Name)
			}
		}
	}
	return nil
}

func (s *HTTPRequestSpec) validateEnvironments() error {
	seen := map[string]bool{}
	for _, e := range s.Environments {
		if err := ValidateName(e); err != nil {
			return fmt.Errorf("spec.environments: %w", err)
		}
		if seen[e] {
			return fmt.Errorf("spec.environments lists %q twice", e)
		}
		seen[e] = true
	}
	if s.DefaultEnvironment != "" {
		if err := ValidateName(s.DefaultEnvironment); err != nil {
			return fmt.Errorf("spec.defaultEnvironment: %w", err)
		}
		if len(s.Environments) > 0 && !seen[s.DefaultEnvironment] {
			return fmt.Errorf("spec.defaultEnvironment %q is not in spec.environments", s.DefaultEnvironment)
		}
	}
	return nil
}

// EnvironmentRefs lists the environments this request declares, which apply
// uses to check they exist.
func (s *HTTPRequestSpec) EnvironmentRefs() []string {
	out := append([]string(nil), s.Environments...)
	if s.DefaultEnvironment != "" {
		found := false
		for _, e := range out {
			if e == s.DefaultEnvironment {
				found = true
				break
			}
		}
		if !found {
			out = append(out, s.DefaultEnvironment)
		}
	}
	sort.Strings(out)
	return out
}

// SelectEnvironment picks which environment to run against.
func (s *HTTPRequestSpec) SelectEnvironment(requested string) (string, error) {
	if requested != "" {
		if len(s.Environments) > 0 {
			for _, e := range s.Environments {
				if e == requested {
					return requested, nil
				}
			}
			return "", fmt.Errorf("environment %q is not one of %s", requested, strings.Join(s.Environments, ", "))
		}
		return requested, nil
	}
	if s.DefaultEnvironment != "" {
		return s.DefaultEnvironment, nil
	}
	if len(s.Environments) == 1 {
		return s.Environments[0], nil
	}
	if len(s.Environments) > 1 {
		return "", fmt.Errorf("this request runs against %s; pick one, there is no default", strings.Join(s.Environments, ", "))
	}
	return "", nil
}

// RequiredParams lists the parameters a caller must supply.
func (s *HTTPRequestSpec) RequiredParams() []string {
	var out []string
	for _, p := range s.PathParams {
		// Every path parameter is required: the url cannot be built without
		// it, whatever the document says.
		out = append(out, p.Name)
	}
	for _, q := range s.Query {
		if q.Required {
			out = append(out, q.Name)
		}
	}
	sort.Strings(out)
	return out
}
