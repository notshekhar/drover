// Package lsp speaks the Language Server Protocol to servers running against
// drover's checkouts.
//
// It models the slice drover needs: the handshake, read-only document sync,
// the navigation requests behind the `lsp` tool, and both diagnostic
// mechanisms. Everything else a language server can do involves changing code,
// which drover does not do.
package lsp

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// Position is a point in a document. LSP counts from zero at both ends; the
// tool's callers count from one, and the conversion happens at the edge.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a span.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range in a named document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// LocationLink is the richer answer some servers give instead of a Location.
// Both shapes arrive from the same requests, so both are decoded.
type LocationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// DocumentSymbol is one entry in a file's outline. It nests.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is the flat shape, returned by workspace/symbol and by
// older servers for documentSymbol.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// CallHierarchyItem is one node of a call graph.
type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Detail         string `json:"detail,omitempty"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}

// CallHierarchyIncomingCall is a caller, plus where it calls from.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall is a callee, plus where it is called.
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// Diagnostic is one problem a server reports.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"` // 1 error, 2 warning, 3 info, 4 hint
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// PublishDiagnosticsParams is the push-model notification.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     int          `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// SeverityLabel names a severity for a human.
func SeverityLabel(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	}
	return "note"
}

// SymbolKind names the LSP kind numbers, so an outline reads as prose rather
// than as "kind:12".
var SymbolKind = map[int]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
	6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
	11: "interface", 12: "function", 13: "variable", 14: "constant",
	15: "string", 16: "number", 17: "boolean", 18: "array", 19: "object",
	20: "key", 21: "null", 22: "enum member", 23: "struct", 24: "event",
	25: "operator", 26: "type parameter",
}

// KindName names a symbol kind, falling back to the number rather than to
// nothing -- an unknown kind is still information.
func KindName(kind int) string {
	if name, ok := SymbolKind[kind]; ok {
		return name
	}
	return "symbol"
}

// PathToURI turns an absolute path into a file:// URI.
//
// url.URL does the escaping, because a path with a space or a '#' in it
// produces a URI that servers silently fail to match against the one they
// were sent -- and the failure looks like "no definition found".
func PathToURI(abs string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// URIToPath turns a file:// URI back into a path.
func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return strings.TrimPrefix(uri, "file://")
	}
	return filepath.FromSlash(u.Path)
}

// LanguageID is the languageId a server expects in didOpen. Getting it wrong
// makes a server refuse the document without saying so.
func LanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".go":
		return "go"
	case ".java":
		return "java"
	}
	return "plaintext"
}

// ToLocations normalises what the position requests actually return.
//
// definition, implementation and typeDefinition may answer with a single
// object or an array, holding either a Location or a LocationLink -- and a
// server may send Locations even after being told linkSupport is on, which
// gopls does. Decoding only the shape that was asked for produces an empty
// result that reads as "no definition found", so accept all four.
func ToLocations(raw []byte) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err != nil {
		many = []json.RawMessage{raw}
	}

	out := make([]Location, 0, len(many))
	for _, item := range many {
		var both struct {
			URI                  string `json:"uri"`
			Range                *Range `json:"range"`
			TargetURI            string `json:"targetUri"`
			TargetRange          *Range `json:"targetRange"`
			TargetSelectionRange *Range `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(item, &both); err != nil {
			continue
		}
		switch {
		case both.URI != "" && both.Range != nil:
			out = append(out, Location{URI: both.URI, Range: *both.Range})
		case both.TargetURI != "":
			// The selection range is the name; the target range is the whole
			// declaration body. The name is what a reader wants pointed at.
			r := both.TargetSelectionRange
			if r == nil {
				r = both.TargetRange
			}
			if r != nil {
				out = append(out, Location{URI: both.TargetURI, Range: *r})
			}
		}
	}
	return out
}
