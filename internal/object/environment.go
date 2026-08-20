package object

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvironmentSpec is one named stage: local, stage, prod. It exists so an
// HTTPRequest is written once instead of copy-pasted per host.
type EnvironmentSpec struct {
	Description string `yaml:"description,omitempty"`

	// Variables are plain values. Safe to print and safe to show an agent.
	Variables map[string]string `yaml:"variables,omitempty"`

	// Secrets must be ${ENV} references, never literals. They are redacted in
	// get output and never sent to an MCP client.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}

// Environment decodes this object's spec as an EnvironmentSpec.
func (o *Object) Environment() (*EnvironmentSpec, error) {
	if o.Kind != KindEnvironment {
		return nil, fmt.Errorf("object is %s, not %s", o.Kind, KindEnvironment)
	}
	var spec EnvironmentSpec
	if err := o.decodeSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate checks the rules that keep a secret from being committed.
func (s *EnvironmentSpec) Validate() error {
	if len(s.Variables) == 0 && len(s.Secrets) == 0 {
		return fmt.Errorf("spec needs variables or secrets; an environment with neither configures nothing")
	}

	for _, name := range sortedKeys(s.Variables) {
		if err := validateVarName(name); err != nil {
			return fmt.Errorf("spec.variables: %w", err)
		}
	}

	for _, name := range sortedKeys(s.Secrets) {
		if err := validateVarName(name); err != nil {
			return fmt.Errorf("spec.secrets: %w", err)
		}
		value := strings.TrimSpace(s.Secrets[name])
		// A literal here would be a credential in a file people commit. The
		// whole point of the secrets block is that it holds references.
		if !isSingleProcessEnvRef(value) {
			return fmt.Errorf("spec.secrets.%s must be a single ${ENV_VAR} reference, not a literal value (put non-secret values in spec.variables)", name)
		}
	}

	// A name in both blocks is ambiguous, and the ambiguity would be resolved
	// silently at request time.
	for _, name := range sortedKeys(s.Secrets) {
		if _, clash := s.Variables[name]; clash {
			return fmt.Errorf("%q is in both spec.variables and spec.secrets", name)
		}
	}
	return nil
}

// isSingleProcessEnvRef reports whether value is exactly one ${NAME} and
// nothing else. "Bearer ${TOKEN}" is refused: half a secret in plain text is
// still a leak of the other half's shape, and the concatenation belongs in
// the request that uses it.
func isSingleProcessEnvRef(value string) bool {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return false
	}
	inner := value[2 : len(value)-1]
	if inner == "" || strings.ContainsAny(inner, "${}") {
		return false
	}
	return true
}

// processEnvRefName returns the NAME in a ${NAME} reference.
func processEnvRefName(value string) string {
	if !isSingleProcessEnvRef(strings.TrimSpace(value)) {
		return ""
	}
	v := strings.TrimSpace(value)
	return v[2 : len(v)-1]
}

func validateVarName(name string) error {
	if name == "" {
		return fmt.Errorf("a name is empty")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("%q contains %q; names may use letters, digits, underscore and dash", name, string(c))
		}
	}
	return nil
}

// Lookup resolves a {{name}} against this environment: variables first, then
// secrets, whose ${ENV} reference is read from the process environment.
//
// getenv is injected so tests do not have to mutate the real environment.
func (s *EnvironmentSpec) Lookup(name string, getenv func(string) (string, bool)) (string, bool) {
	if v, ok := s.Variables[name]; ok {
		return v, true
	}
	ref, ok := s.Secrets[name]
	if !ok {
		return "", false
	}
	envName := processEnvRefName(ref)
	if envName == "" {
		return "", false
	}
	if getenv == nil {
		getenv = osLookupEnv
	}
	return getenv(envName)
}

func osLookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// SecretNames lists the secret keys, sorted.
func (s *EnvironmentSpec) SecretNames() []string { return sortedKeys(s.Secrets) }

// IsSecret reports whether a name is a secret, so callers know to redact it.
func (s *EnvironmentSpec) IsSecret(name string) bool {
	_, ok := s.Secrets[name]
	return ok
}

// SecretStatus describes a secret without revealing it: which env var backs
// it, and whether that var is actually set.
type SecretStatus struct {
	Name    string `json:"name" yaml:"name"`
	FromEnv string `json:"fromEnv" yaml:"fromEnv"`
	Set     bool   `json:"set" yaml:"set"`
}

// SecretStatuses reports every secret's backing variable and whether it is
// present, which is what `drover get environment` prints instead of values.
func (s *EnvironmentSpec) SecretStatuses(getenv func(string) (string, bool)) []SecretStatus {
	if getenv == nil {
		getenv = osLookupEnv
	}
	var out []SecretStatus
	for _, name := range s.SecretNames() {
		envName := processEnvRefName(s.Secrets[name])
		_, set := getenv(envName)
		out = append(out, SecretStatus{Name: name, FromEnv: envName, Set: set})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
