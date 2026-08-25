// .bru parsing, vendored from bruh (github.com/notshekhar/bruh,
// internal/bru/parse.go) where it is verified against a real collection at
// 223 of 223 files.
//
// Copied rather than imported: bruh is a separate module and this is a
// standalone, dependency-free reader. Copying it keeps drover's one-binary,
// no-extra-dependency shape, at the cost of two copies that can drift. The
// drift is acceptable because the .bru grammar does not move.
//
// This is a reader, not a writer. .bru only has to be understood well enough
// to convert once, which is why there is no serialiser here and no attempt at
// byte-faithful round-tripping.
//
// The grammar is small. A file is a sequence of blocks, and a block is one of
// three shapes:
//
//	dictionary   headers { name: value }        pairs, one per line
//	text         body:json { …raw text… }       everything until the closing brace
//	list         tags [ one, two ]              bare items
//
// On top of that: a `~` prefix on a key disables that row, `@annotations` sit
// on the line above a pair and carry descriptions and types, and `”'` fences a
// multi-line value.
package importer

import (
	"fmt"
	"strings"
)

// Block is one parsed block.
type Block struct {
	// Name is the block's keyword, e.g. "meta", "headers", "auth:bearer".
	Name string
	// Kind is dict, text or list.
	Kind string
	// Pairs is set for dict blocks, in file order.
	Pairs []Pair
	// Text is set for text blocks.
	Text string
	// Items is set for list blocks.
	Items []string
}

// Block kinds.
const (
	KindDict = "dict"
	KindText = "text"
	KindList = "list"
)

// Pair is one key/value row of a dictionary block.
type Pair struct {
	Key   string
	Value string
	// Disabled is set when the key was written with a leading `~`.
	Disabled bool
	// Description comes from an @description annotation.
	Description string
	// Type comes from an @number/@boolean/@object annotation.
	Type string
}

// textBlocks hold raw content rather than pairs. Everything not listed here is
// parsed as a dictionary unless it opens with `[`.
var textBlocks = map[string]bool{
	"body":                 true,
	"body:json":            true,
	"body:text":            true,
	"body:xml":             true,
	"body:sparql":          true,
	"body:graphql":         true,
	"body:graphql:vars":    true,
	"script:pre-request":   true,
	"script:post-response": true,
	"tests":                true,
	"docs":                 true,
}

// ParseBru splits a .bru file into blocks.
func ParseBru(source string) ([]Block, error) {
	p := &parser{src: []rune(strings.ReplaceAll(source, "\r\n", "\n"))}
	return p.parse()
}

type parser struct {
	src []rune
	pos int
}

func (p *parser) parse() ([]Block, error) {
	var blocks []Block
	for {
		p.skipSpaceAndComments()
		if p.pos >= len(p.src) {
			return blocks, nil
		}
		name := p.readName()
		if name == "" {
			return nil, fmt.Errorf("expected a block name at offset %d, got %q", p.pos, p.peekLine())
		}
		p.skipInlineSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("block %q has no body", name)
		}

		switch p.src[p.pos] {
		case '{':
			p.pos++
			if textBlocks[name] {
				text, err := p.readTextBlock()
				if err != nil {
					return nil, fmt.Errorf("block %q: %w", name, err)
				}
				blocks = append(blocks, Block{Name: name, Kind: KindText, Text: text})
				continue
			}
			pairs, err := p.readDict()
			if err != nil {
				return nil, fmt.Errorf("block %q: %w", name, err)
			}
			blocks = append(blocks, Block{Name: name, Kind: KindDict, Pairs: pairs})
		case '[':
			p.pos++
			items, err := p.readList()
			if err != nil {
				return nil, fmt.Errorf("block %q: %w", name, err)
			}
			blocks = append(blocks, Block{Name: name, Kind: KindList, Items: items})
		default:
			return nil, fmt.Errorf("block %q: expected { or [, got %q", name, string(p.src[p.pos]))
		}
	}
}

func (p *parser) peekLine() string {
	end := p.pos
	for end < len(p.src) && p.src[end] != '\n' {
		end++
	}
	return string(p.src[p.pos:minInt(end, p.pos+40)])
}

func (p *parser) skipInlineSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) skipSpaceAndComments() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' {
			p.pos++
			continue
		}
		// A line comment is only a comment at the start of a line; `//` inside a
		// URL or a script is content.
		if c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/' && p.atLineStart() {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *parser) atLineStart() bool {
	for i := p.pos - 1; i >= 0; i-- {
		switch p.src[i] {
		case '\n':
			return true
		case ' ', '\t':
			continue
		default:
			return false
		}
	}
	return true
}

// readName reads a block keyword: letters, digits and the separators that
// appear in names like `auth:oauth2:additional_params:auth_req:headers`.
func (p *parser) readName() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == ':' || c == '-' || c == '_' {
			p.pos++
			continue
		}
		break
	}
	return string(p.src[start:p.pos])
}

// readTextBlock reads until the closing brace, which must sit at column 0
// immediately after a newline.
//
// That precise rule is load-bearing and matches Bruno's own grammar
// (`tagend = nl "}"`). The content of these blocks is a JSON body or a
// JavaScript function, so it is full of braces that are not the terminator —
// and every one of them is indented, because it is nested inside the block.
// Accepting an indented `}` ends the block at the first `  }` of the payload,
// which silently truncates almost every real file.
//
// Brace counting is not an option either: a body can contain an unbalanced
// brace inside a string, and a script can contain one inside a regex.
func (p *parser) readTextBlock() (string, error) {
	// Skip the rest of the opening line.
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
	if p.pos < len(p.src) {
		p.pos++
	}
	start := p.pos

	for p.pos < len(p.src) {
		if p.src[p.pos] != '\n' {
			p.pos++
			continue
		}
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == '}' {
			text := string(p.src[start:p.pos])
			p.pos += 2 // past the newline and the brace
			return outdent(text), nil
		}
		p.pos++
	}
	return "", fmt.Errorf("unterminated block: no closing brace at the start of a line")
}

func (p *parser) readDict() ([]Pair, error) {
	var pairs []Pair
	var pendingAnnotations []annotation

	for {
		p.skipSpaceAndComments()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated block")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return pairs, nil
		}

		// An annotation sits on its own line above the pair it describes.
		if p.src[p.pos] == '@' {
			a, err := p.readAnnotation()
			if err != nil {
				return nil, err
			}
			pendingAnnotations = append(pendingAnnotations, a)
			continue
		}

		key, err := p.readKey()
		if err != nil {
			return nil, err
		}
		p.skipInlineSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("expected ':' after key %q", key)
		}
		p.pos++
		p.skipInlineSpace()

		value, err := p.readValue()
		if err != nil {
			return nil, err
		}

		pair := Pair{Key: key, Value: value}
		if strings.HasPrefix(pair.Key, "~") {
			pair.Key = pair.Key[1:]
			pair.Disabled = true
		}
		for _, a := range pendingAnnotations {
			switch a.name {
			case "description":
				pair.Description = a.arg
			case "number", "boolean", "object", "string":
				pair.Type = a.name
			}
		}
		pendingAnnotations = nil
		pairs = append(pairs, pair)
	}
}

// readKey reads up to the colon. A quoted key may contain colons and spaces,
// which is how a header called `X-Weird: Thing` is expressible at all.
func (p *parser) readKey() (string, error) {
	disabled := false
	if p.pos < len(p.src) && p.src[p.pos] == '~' {
		disabled = true
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '"' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.src) && p.src[p.pos] != '"' {
			if p.src[p.pos] == '\\' && p.pos+1 < len(p.src) {
				p.pos++
			}
			b.WriteRune(p.src[p.pos])
			p.pos++
		}
		p.pos++ // closing quote
		key := b.String()
		if disabled {
			key = "~" + key
		}
		return key, nil
	}

	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != ':' && p.src[p.pos] != '\n' {
		p.pos++
	}
	key := strings.TrimSpace(string(p.src[start:p.pos]))
	if disabled {
		key = "~" + key
	}
	return key, nil
}

func (p *parser) readValue() (string, error) {
	// A ''' fence holds a multi-line value.
	if p.pos+2 < len(p.src) && string(p.src[p.pos:p.pos+3]) == "'''" {
		p.pos += 3
		start := p.pos
		for p.pos+2 < len(p.src) && string(p.src[p.pos:p.pos+3]) != "'''" {
			p.pos++
		}
		if p.pos+2 >= len(p.src) {
			return "", fmt.Errorf("unterminated ''' value")
		}
		text := string(p.src[start:p.pos])
		p.pos += 3
		// Skip a trailing @contentType() annotation and the rest of the line.
		for p.pos < len(p.src) && p.src[p.pos] != '\n' {
			p.pos++
		}
		return outdent(strings.Trim(text, "\n")), nil
	}

	// A value can itself be a list spanning several lines, which is how `meta`
	// carries tags. Reading to the end of the line here would leave the items
	// to be parsed as keys, and the next one has no colon.
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		p.pos++
		items, err := p.readList()
		if err != nil {
			return "", err
		}
		return strings.Join(items, ","), nil
	}

	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
	return strings.TrimSpace(string(p.src[start:p.pos])), nil
}

func (p *parser) readList() ([]string, error) {
	var items []string
	var cur strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ']' {
			p.pos++
			if item := strings.TrimSpace(cur.String()); item != "" {
				items = append(items, item)
			}
			return items, nil
		}
		if c == ',' || c == '\n' {
			if item := strings.TrimSpace(cur.String()); item != "" {
				items = append(items, item)
			}
			cur.Reset()
			p.pos++
			continue
		}
		cur.WriteRune(c)
		p.pos++
	}
	return nil, fmt.Errorf("unterminated list")
}

type annotation struct {
	name string
	arg  string
}

func (p *parser) readAnnotation() (annotation, error) {
	p.pos++ // @
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '(' && p.src[p.pos] != '\n' {
		p.pos++
	}
	name := strings.TrimSpace(string(p.src[start:p.pos]))
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return annotation{name: name}, nil
	}
	p.pos++ // (

	// The argument may itself be a ''' block, which can contain parentheses.
	if p.pos+2 < len(p.src) && string(p.src[p.pos:p.pos+3]) == "'''" {
		p.pos += 3
		argStart := p.pos
		for p.pos+2 < len(p.src) && string(p.src[p.pos:p.pos+3]) != "'''" {
			p.pos++
		}
		arg := outdent(strings.Trim(string(p.src[argStart:p.pos]), "\n"))
		p.pos += 3
		for p.pos < len(p.src) && p.src[p.pos] != '\n' {
			p.pos++
		}
		return annotation{name: name, arg: arg}, nil
	}

	depth := 1
	argStart := p.pos
	for p.pos < len(p.src) && depth > 0 {
		switch p.src[p.pos] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				arg := string(p.src[argStart:p.pos])
				p.pos++
				for p.pos < len(p.src) && p.src[p.pos] != '\n' {
					p.pos++
				}
				return annotation{name: name, arg: unquote(arg)}, nil
			}
		}
		p.pos++
	}
	return annotation{name: name}, nil
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// outdent removes the common leading indentation. Block contents are written
// indented inside their braces, and keeping that indentation would put two
// extra spaces on every line of every script and JSON body.
func outdent(text string) string {
	lines := strings.Split(text, "\n")
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return strings.TrimRight(text, "\n \t")
	}
	for i, line := range lines {
		if len(line) >= indent {
			lines[i] = line[indent:]
		} else {
			lines[i] = strings.TrimLeft(line, " \t")
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n \t")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
