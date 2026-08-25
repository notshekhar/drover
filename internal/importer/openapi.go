// Package importer turns collections people already have -- OpenAPI specs,
// Bruno collections -- into drover documents.
//
// It generates yaml and nothing else. There is no second runtime path: what
// comes out is an ordinary document a human can read, edit, commit and apply,
// which is why a spec with a shape drover cannot express is a warning rather
// than a silent mistranslation.
package importer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/object"
)

// Options shape one import.
type Options struct {
	// Prefix disambiguates names when several collections land in one engine.
	// "github" turns get-user into github-get-user.
	Prefix string

	// Environment is the name of the Environment document to generate, and
	// the one every request defaults to.
	Environment string

	// Tags narrows an import to operations carrying one of these tags.
	Tags []string

	// All lifts the refusal on a large spec. Without it, an import that would
	// produce more than Threshold requests reports the count and stops.
	All bool
}

// Threshold is how many requests an import will produce before it insists the
// caller says they meant it.
//
// A public API spec routinely has four hundred operations. Turning all of
// them into documents is not obviously wrong, but it is never what someone
// running the command for the first time wanted, and the fix -- a tag filter
// -- is one flag away.
const Threshold = 40

// Result is what an import produced.
type Result struct {
	Documents []byte
	Requests  int
	Skipped   []string // operations that could not be expressed, with the reason
	Total     int      // operations the spec holds, before filtering
	Truncated bool     // Total exceeded Threshold and All was not set
	Title     string
}

// OpenAPI converts an OpenAPI 3 document into drover objects.
//
// JSON needs no separate path: every JSON document is a YAML document, and
// yaml.Unmarshal reads both.
func OpenAPI(data []byte, opts Options) (*Result, error) {
	var doc oaDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("this is not a readable OpenAPI document: %w", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("the document has no paths; is it an OpenAPI spec?")
	}
	if doc.Swagger != "" && doc.OpenAPI == "" {
		return nil, fmt.Errorf("this is Swagger %s; drover imports OpenAPI 3", doc.Swagger)
	}

	res := &Result{Title: doc.Info.Title}
	env := opts.Environment
	if env == "" {
		env = "default"
	}

	type built struct {
		name string
		spec *object.HTTPRequestSpec
	}
	var requests []built

	for _, path := range sortedKeys(doc.Paths) {
		item := doc.Paths[path]
		for _, method := range sortedKeys(item.Operations()) {
			op := item.Operations()[method]
			res.Total++
			if !matchesTags(op.Tags, opts.Tags) {
				continue
			}

			name := requestName(opts.Prefix, op.OperationID, method, path)
			if err := object.ValidateName(name); err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: %v", method, path, err))
				continue
			}
			spec, err := buildRequest(&doc, item, op, method, path, env)
			if err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s %s: %v", method, path, err))
				continue
			}
			requests = append(requests, built{name: name, spec: spec})
		}
	}

	if len(requests) > Threshold && !opts.All {
		res.Truncated = true
		res.Requests = len(requests)
		return res, nil
	}

	var out strings.Builder
	envDoc, err := environmentDocument(env, doc.ServerURL())
	if err != nil {
		return nil, err
	}
	out.Write(envDoc)

	seen := map[string]bool{}
	for _, r := range requests {
		if seen[r.name] {
			res.Skipped = append(res.Skipped, r.name+": a second operation wants the same name")
			continue
		}
		seen[r.name] = true
		doc, err := requestDocument(r.name, r.spec)
		if err != nil {
			return nil, err
		}
		out.WriteString("---\n")
		out.Write(doc)
		res.Requests++
	}
	res.Documents = []byte(out.String())
	return res, nil
}

// buildRequest maps one operation onto an HTTPRequestSpec.
func buildRequest(doc *oaDocument, item *oaPathItem, op *oaOperation, method, path, env string) (*object.HTTPRequestSpec, error) {
	spec := &object.HTTPRequestSpec{
		Description:        describe(op),
		Method:             strings.ToUpper(method),
		URL:                "{{baseUrl}}" + path,
		Environments:       []string{env},
		DefaultEnvironment: env,
	}

	// Path-level parameters apply to every operation under it, and are
	// overridden by an operation's own. Missing this is why generated
	// requests come out short a required id.
	params := append(append([]*oaParameter{}, item.Parameters...), op.Parameters...)

	inPath := map[string]bool{}
	for _, name := range pathPlaceholders(path) {
		inPath[name] = true
	}

	byName := map[string]bool{}
	for _, raw := range params {
		p, err := doc.resolveParameter(raw)
		if err != nil {
			return nil, err
		}
		if p == nil || p.Name == "" || byName[p.In+"/"+p.Name] {
			continue
		}
		byName[p.In+"/"+p.Name] = true

		entry := object.Param{
			Name:        p.Name,
			Description: p.describeOrDerive(),
			Required:    p.Required,
			Example:     p.exampleString(),
		}
		switch p.In {
		case "path":
			entry.Required = true // a path parameter is required by definition
			spec.PathParams = append(spec.PathParams, entry)
			delete(inPath, p.Name)
		case "query":
			spec.Query = append(spec.Query, entry)
		case "header", "cookie":
			// A caller-settable header is a credential-theft and
			// request-smuggling primitive, and drover refuses one by design.
			// Dropping it silently would produce a request that looks
			// complete and is not, so it is reported.
			return nil, fmt.Errorf("parameter %q is in %s, which drover does not let a caller set", p.Name, p.In)
		}
	}

	// Every {name} in the url must have a declared parameter, or apply will
	// reject the document. A spec that templates a segment it never declares
	// is common enough to be worth filling in rather than failing.
	for name := range inPath {
		spec.PathParams = append(spec.PathParams, object.Param{
			Name:        name,
			Description: "Undeclared in the spec; drover inferred it from the url.",
			Required:    true,
		})
	}
	sort.Slice(spec.PathParams, func(i, j int) bool { return spec.PathParams[i].Name < spec.PathParams[j].Name })
	sort.Slice(spec.Query, func(i, j int) bool { return spec.Query[i].Name < spec.Query[j].Name })
	return spec, nil
}

func describe(op *oaOperation) string {
	for _, s := range []string{op.Summary, op.Description} {
		if t := strings.TrimSpace(s); t != "" {
			return collapseLines(t)
		}
	}
	return ""
}

// collapseLines keeps a description to one paragraph. api_list searches these,
// and a four-page markdown description drowns the search.
func collapseLines(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:297] + "..."
	}
	return s
}

var (
	placeholderPattern = regexp.MustCompile(`\{([^{}/]+)\}`)
	// environmentPattern is drover's other placeholder syntax, and it has to
	// be removed before scanning for the first: {{baseUrl}} contains
	// {baseUrl}, so a naive scan declares a path parameter for every
	// environment variable in the url.
	environmentPattern = regexp.MustCompile(`\{\{[^{}]*\}\}`)
)

// pathPlaceholders returns the {name} parameters in a url -- the only syntax
// that becomes a caller-settable tool parameter.
func pathPlaceholders(path string) []string {
	path = environmentPattern.ReplaceAllString(path, "")
	var out []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

// requestName turns an operation into a drover name.
//
// operationId when there is one, because it is what the API's own authors
// called it; otherwise the method and path, which is ugly but stable.
func requestName(prefix, operationID, method, path string) string {
	base := operationID
	if strings.TrimSpace(base) == "" {
		base = method + "-" + strings.ReplaceAll(strings.Trim(path, "/"), "/", "-")
	}
	name := kebab(base)
	if prefix != "" {
		name = kebab(prefix) + "-" + name
	}
	if len(name) > object.MaxNameLen {
		name = strings.Trim(name[:object.MaxNameLen], "-")
	}
	return name
}

// kebab lowercases and separates, handling the camelCase operationIds that
// generated specs are full of: getUserById becomes get-user-by-id.
func kebab(s string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('-')
			}
			b.WriteRune(r + 32)
			prevLower = false
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = r >= 'a' && r <= 'z'
		default:
			b.WriteByte('-')
			prevLower = false
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

func matchesTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		for _, h := range have {
			if strings.EqualFold(h, w) {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
