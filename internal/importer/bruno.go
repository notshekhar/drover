package importer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/notshekhar/drover/internal/object"
)

// Bruno converts a Bruno collection directory into drover documents.
//
// A collection is a tree of .bru files plus environments/*.bru. Both become
// what they already are in drover's model: a request is an HTTPRequest, an
// environment file is an Environment, and the collection's own vars merge
// into every environment as defaults.
func Bruno(dir string, opts Options) (*Result, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file; point this at the collection directory", dir)
	}

	res := &Result{Title: filepath.Base(dir)}
	envs, collectionVars, err := brunoEnvironments(dir)
	if err != nil {
		return nil, err
	}
	envNames := sortedKeys(envs)

	type built struct {
		name string
		spec *object.HTTPRequestSpec
	}
	var requests []built

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".bru") {
			return nil
		}
		// environments/ and the collection file are not requests.
		if rel, _ := filepath.Rel(dir, path); strings.HasPrefix(rel, "environments"+string(filepath.Separator)) {
			return nil
		}
		if d.Name() == "collection.bru" || d.Name() == "folder.bru" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		blocks, err := ParseBru(string(data))
		if err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", d.Name(), err))
			return nil
		}
		res.Total++

		spec, tags, err := brunoRequest(blocks, envNames)
		if err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", d.Name(), err))
			return nil
		}
		if !matchesTags(tags, opts.Tags) {
			return nil
		}
		name := requestName(opts.Prefix, blockValue(blocks, "meta", "name"), spec.Method, strings.TrimSuffix(d.Name(), ".bru"))
		if err := object.ValidateName(name); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", d.Name(), err))
			return nil
		}
		requests = append(requests, built{name: name, spec: spec})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(requests) > Threshold && !opts.All {
		res.Truncated, res.Requests = true, len(requests)
		return res, nil
	}

	var out strings.Builder
	for _, name := range envNames {
		vars := map[string]string{}
		for k, v := range collectionVars {
			vars[k] = v
		}
		for k, v := range envs[name] {
			vars[k] = v
		}
		doc, err := brunoEnvironmentDocument(name, vars)
		if err != nil {
			return nil, err
		}
		if out.Len() > 0 {
			out.WriteString("---\n")
		}
		out.Write(doc)
	}

	sort.Slice(requests, func(i, j int) bool { return requests[i].name < requests[j].name })
	seen := map[string]bool{}
	for _, r := range requests {
		if seen[r.name] {
			res.Skipped = append(res.Skipped, r.name+": a second request wants the same name")
			continue
		}
		seen[r.name] = true
		doc, err := requestDocument(r.name, r.spec)
		if err != nil {
			return nil, err
		}
		if out.Len() > 0 {
			out.WriteString("---\n")
		}
		out.Write(doc)
		res.Requests++
	}
	res.Documents = []byte(out.String())
	return res, nil
}

// brunoRequest maps one parsed .bru file onto an HTTPRequestSpec.
func brunoRequest(blocks []Block, envNames []string) (*object.HTTPRequestSpec, []string, error) {
	method, url := "", ""
	for _, b := range blocks {
		// Bruno names the method block after the method: get { url: … }.
		if b.Kind == KindDict && object.SafeMethods[strings.ToUpper(b.Name)] || isMethodBlock(b.Name) {
			method = strings.ToUpper(b.Name)
			url = pairValue(b.Pairs, "url")
		}
	}
	if method == "" || url == "" {
		return nil, nil, fmt.Errorf("no method block with a url")
	}

	spec := &object.HTTPRequestSpec{
		Method:      method,
		URL:         brunoURL(url),
		Description: collapseLines(blockText(blocks, "docs")),
	}
	if len(envNames) > 0 {
		spec.Environments = envNames
		spec.DefaultEnvironment = envNames[0]
	}

	for _, b := range blocks {
		switch {
		case b.Name == "params:path" && b.Kind == KindDict:
			for _, p := range b.Pairs {
				if p.Disabled {
					continue
				}
				spec.PathParams = append(spec.PathParams, object.Param{
					Name: p.Key, Description: brunoDescription(p), Required: true, Example: p.Value,
				})
			}
		case b.Name == "params:query" && b.Kind == KindDict:
			for _, p := range b.Pairs {
				if p.Disabled {
					continue
				}
				spec.Query = append(spec.Query, object.Param{
					Name: p.Key, Description: brunoDescription(p), Example: p.Value,
				})
			}
		case b.Name == "headers" && b.Kind == KindDict:
			for _, p := range b.Pairs {
				if p.Disabled {
					continue
				}
				spec.Headers = append(spec.Headers, object.Header{
					Name: p.Key, Value: brunoInterpolation(p.Value), Description: p.Description,
				})
			}
		}
	}

	// Every {name} in the url needs a declared parameter or apply refuses the
	// document. Bruno files routinely template a segment and declare it
	// nowhere.
	declared := map[string]bool{}
	for _, p := range spec.PathParams {
		declared[p.Name] = true
	}
	for _, name := range pathPlaceholders(spec.URL) {
		if !declared[name] {
			spec.PathParams = append(spec.PathParams, object.Param{
				Name:        name,
				Description: "Undeclared in the collection; drover inferred it from the url.",
				Required:    true,
			})
		}
	}
	sort.Slice(spec.PathParams, func(i, j int) bool { return spec.PathParams[i].Name < spec.PathParams[j].Name })
	sort.Slice(spec.Query, func(i, j int) bool { return spec.Query[i].Name < spec.Query[j].Name })

	var tags []string
	for _, b := range blocks {
		if b.Name == "tags" && b.Kind == KindList {
			tags = append(tags, b.Items...)
		}
	}
	return spec, tags, nil
}

// brunoDescription is the description drover will store.
//
// drover refuses a parameter with no description, because the description is
// what an agent reads to decide what to put in the field. Bruno carries one
// only when somebody wrote an @description annotation, which is rare, so the
// fallback says what the collection did give: the value it was last called
// with, which is a genuinely useful hint about the shape of the field.
func brunoDescription(p Pair) string {
	if d := strings.TrimSpace(p.Description); d != "" {
		return collapseLines(d)
	}
	if v := strings.TrimSpace(p.Value); v != "" && !strings.Contains(v, "{{") {
		return "The collection calls this with " + v + "."
	}
	return "The collection gives no description for this parameter."
}

func isMethodBlock(name string) bool {
	switch strings.ToUpper(name) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// brunoURL rewrites Bruno's placeholder syntaxes into drover's.
//
// Bruno writes {{var}} for an environment variable and :id for a path
// segment. drover writes {{var}} for the first -- the same -- and {id} for
// the second, because {id} is the only syntax that becomes a tool parameter.
func brunoURL(raw string) string {
	// Strip a query string: drover carries query parameters as declarations,
	// and leaving them in the url would send them twice.
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	parts := strings.Split(raw, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") && len(p) > 1 {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

// brunoInterpolation leaves {{var}} alone -- it means the same thing in both
// tools -- and that is the whole conversion.
func brunoInterpolation(v string) string { return v }

// brunoEnvironments reads environments/*.bru, plus the collection's own vars.
func brunoEnvironments(dir string) (map[string]map[string]string, map[string]string, error) {
	envs := map[string]map[string]string{}
	collectionVars := map[string]string{}

	if data, err := os.ReadFile(filepath.Join(dir, "collection.bru")); err == nil {
		if blocks, err := ParseBru(string(data)); err == nil {
			for _, b := range blocks {
				if strings.HasPrefix(b.Name, "vars") && b.Kind == KindDict {
					for _, p := range b.Pairs {
						if !p.Disabled {
							collectionVars[p.Key] = p.Value
						}
					}
				}
			}
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "environments"))
	if err != nil {
		return envs, collectionVars, nil // a collection without environments is fine
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bru") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "environments", e.Name()))
		if err != nil {
			continue
		}
		blocks, err := ParseBru(string(data))
		if err != nil {
			continue
		}
		name := kebab(strings.TrimSuffix(e.Name(), ".bru"))
		if object.ValidateName(name) != nil {
			continue
		}
		vars := map[string]string{}
		for _, b := range blocks {
			if !strings.HasPrefix(b.Name, "vars") || b.Kind != KindDict {
				continue
			}
			for _, p := range b.Pairs {
				if !p.Disabled {
					vars[p.Key] = p.Value
				}
			}
		}
		envs[name] = vars
	}
	return envs, collectionVars, nil
}

// brunoEnvironmentDocument writes one Environment.
//
// Nothing is emitted as a secret. Bruno's secret vars hold literal values,
// and drover refuses a literal in secrets: by design, a secret must be a bare
// ${ENV} reference. Writing them as ordinary variables and letting a person
// move the ones that matter is the honest translation; inventing environment
// variable names for them would not be.
func brunoEnvironmentDocument(name string, vars map[string]string) ([]byte, error) {
	d := &document{APIVersion: object.APIVersion, Kind: object.KindEnvironment}
	d.Metadata.Name = name
	if len(vars) == 0 {
		vars = map[string]string{"baseUrl": "https://example.invalid"}
	}
	d.Spec = map[string]any{"variables": vars}
	return marshal(d)
}

func blockValue(blocks []Block, block, key string) string {
	for _, b := range blocks {
		if b.Name == block && b.Kind == KindDict {
			return pairValue(b.Pairs, key)
		}
	}
	return ""
}

func blockText(blocks []Block, name string) string {
	for _, b := range blocks {
		if b.Name == name && b.Kind == KindText {
			return b.Text
		}
	}
	return ""
}

func pairValue(pairs []Pair, key string) string {
	for _, p := range pairs {
		if p.Key == key && !p.Disabled {
			return p.Value
		}
	}
	return ""
}
