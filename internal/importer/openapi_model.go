package importer

import (
	"fmt"
	"strings"
)

// The subset of OpenAPI 3 drover reads.
//
// Deliberately not a full model. Everything drover can express is here;
// everything else is either ignored (schemas, responses, security schemes) or
// reported as a skip. A partial model that says which parts it read beats a
// complete one that quietly mistranslates.
type oaDocument struct {
	OpenAPI string `yaml:"openapi"`
	Swagger string `yaml:"swagger"` // only to recognise and refuse it
	Info    struct {
		Title string `yaml:"title"`
	} `yaml:"info"`
	Servers []struct {
		URL string `yaml:"url"`
	} `yaml:"servers"`
	Paths      map[string]*oaPathItem `yaml:"paths"`
	Components struct {
		Parameters map[string]*oaParameter `yaml:"parameters"`
	} `yaml:"components"`
}

// ServerURL is the base every generated request hangs off.
func (d *oaDocument) ServerURL() string {
	for _, s := range d.Servers {
		if u := strings.TrimSpace(s.URL); u != "" {
			return strings.TrimSuffix(u, "/")
		}
	}
	return ""
}

type oaPathItem struct {
	Parameters []*oaParameter `yaml:"parameters"`

	Get     *oaOperation `yaml:"get"`
	Put     *oaOperation `yaml:"put"`
	Post    *oaOperation `yaml:"post"`
	Delete  *oaOperation `yaml:"delete"`
	Patch   *oaOperation `yaml:"patch"`
	Head    *oaOperation `yaml:"head"`
	Options *oaOperation `yaml:"options"`
}

// Operations returns the methods this path defines, keyed by method.
func (p *oaPathItem) Operations() map[string]*oaOperation {
	out := map[string]*oaOperation{}
	for method, op := range map[string]*oaOperation{
		"get": p.Get, "put": p.Put, "post": p.Post, "delete": p.Delete,
		"patch": p.Patch, "head": p.Head, "options": p.Options,
	} {
		if op != nil {
			out[method] = op
		}
	}
	return out
}

type oaOperation struct {
	OperationID string         `yaml:"operationId"`
	Summary     string         `yaml:"summary"`
	Description string         `yaml:"description"`
	Tags        []string       `yaml:"tags"`
	Parameters  []*oaParameter `yaml:"parameters"`
}

type oaParameter struct {
	Ref         string `yaml:"$ref"`
	Name        string `yaml:"name"`
	In          string `yaml:"in"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Example     any    `yaml:"example"`
	Schema      *struct {
		Type    string `yaml:"type"`
		Example any    `yaml:"example"`
		Enum    []any  `yaml:"enum"`
		Default any    `yaml:"default"`
		Format  string `yaml:"format"`
	} `yaml:"schema"`
}

// describeOrDerive is the description drover will store.
//
// A parameter with no description is not merely unhelpful here -- drover
// refuses the document, because the description is what an agent reads to
// decide what to put in the field. Rather than fail an import over a spec
// that skipped it, derive one from what the spec *does* say: the type, the
// format, the allowed values, the default. That is real information, not
// invented, and it says plainly when there was nothing to work from.
func (p *oaParameter) describeOrDerive() string {
	if d := strings.TrimSpace(p.Description); d != "" {
		return collapseLines(d)
	}
	var parts []string
	if p.Schema != nil {
		if t := p.Schema.Type; t != "" {
			if p.Schema.Format != "" {
				t += " (" + p.Schema.Format + ")"
			}
			parts = append(parts, t)
		}
		if len(p.Schema.Enum) > 0 {
			var vals []string
			for _, v := range p.Schema.Enum {
				if s := scalarString(v); s != "" {
					vals = append(vals, s)
				}
			}
			if len(vals) > 0 {
				parts = append(parts, "one of: "+strings.Join(vals, ", "))
			}
		}
		if d := scalarString(p.Schema.Default); d != "" {
			parts = append(parts, "defaults to "+d)
		}
	}
	if len(parts) == 0 {
		return "The spec gives no description for this parameter."
	}
	return capitalise(strings.Join(parts, "; ")) + "."
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

// exampleString picks the most useful concrete value the spec offers.
//
// An example is worth more to a model than a type: "usr_1a2b3c" tells it the
// shape of an id, where "string" tells it nothing it did not assume.
func (p *oaParameter) exampleString() string {
	if s := scalarString(p.Example); s != "" {
		return s
	}
	if p.Schema == nil {
		return ""
	}
	if s := scalarString(p.Schema.Example); s != "" {
		return s
	}
	if s := scalarString(p.Schema.Default); s != "" {
		return s
	}
	if len(p.Schema.Enum) > 0 {
		return scalarString(p.Schema.Enum[0])
	}
	return ""
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool, int, int64, float64:
		return fmt.Sprint(t)
	}
	return ""
}

// maxRefDepth bounds $ref resolution.
//
// Generated specs contain cycles -- a parameter that references itself
// through a chain of components is not rare -- and a naive resolver hangs on
// one rather than failing.
const maxRefDepth = 8

// resolveParameter follows a local $ref into components/parameters.
//
// A remote $ref is refused: resolving one means fetching a url out of a
// document drover did not write, which is the same class of thing as letting
// a repository declare its own SQLConnection.
func (d *oaDocument) resolveParameter(p *oaParameter) (*oaParameter, error) {
	for depth := 0; p != nil && p.Ref != ""; depth++ {
		if depth >= maxRefDepth {
			return nil, fmt.Errorf("$ref %q goes round in a circle", p.Ref)
		}
		name, ok := strings.CutPrefix(p.Ref, "#/components/parameters/")
		if !ok {
			if strings.HasPrefix(p.Ref, "#/") {
				return nil, fmt.Errorf("$ref %q does not point at components/parameters", p.Ref)
			}
			return nil, fmt.Errorf("$ref %q is remote, and drover will not fetch it", p.Ref)
		}
		next := d.Components.Parameters[name]
		if next == nil {
			return nil, fmt.Errorf("$ref %q is not in this document", p.Ref)
		}
		p = next
	}
	return p, nil
}
